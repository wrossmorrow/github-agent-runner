package pirpc

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestHelperProcess acts as a scripted fake `pi --mode rpc` subprocess when
// GO_WANT_HELPER_PROCESS=1 is set.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var cmd map[string]any
		if err := json.Unmarshal(sc.Bytes(), &cmd); err != nil {
			continue
		}
		id, _ := cmd["id"].(string)
		switch cmd["type"] {
		case "get_state":
			_ = enc.Encode(map[string]any{
				"type":    "response",
				"id":      id,
				"success": true,
				"data":    map[string]any{"sessionId": "sess-123"},
			})
		case "get_session_stats":
			_ = enc.Encode(map[string]any{
				"type":    "response",
				"id":      id,
				"success": true,
				"data":    map[string]any{"turns": 1, "totalTokens": 42},
			})
		case "prompt":
			_ = enc.Encode(map[string]any{
				"type":    "response",
				"id":      id,
				"success": true,
			})
			_ = enc.Encode(map[string]any{
				"type":    "message_update",
				"message": map[string]any{"huge": "dropped"},
				"assistantMessageEvent": map[string]any{
					"type":         "toolcall_start",
					"contentIndex": 0,
					"partial": map[string]any{
						"content": []any{
							map[string]any{"type": "toolCall", "name": "read", "id": "tc-1"},
						},
					},
				},
			})
			_ = enc.Encode(map[string]any{
				"type": "agent_end",
				"messages": []any{
					map[string]any{"role": "assistant", "content": "done"},
				},
			})
		case "fail":
			_ = enc.Encode(map[string]any{
				"type":    "response",
				"id":      id,
				"success": false,
				"error":   "intentional failure",
			})
		default:
			_ = enc.Encode(map[string]any{
				"type":    "response",
				"id":      id,
				"success": false,
				"error":   "unknown command",
			})
		}
	}
	os.Exit(0)
}

func startHelperClient(t *testing.T) *Client {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	c := NewClient(exe, []string{"-test.run=TestHelperProcess"}, "", env)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.CloseStdin()
		_ = c.Stop()
	})
	return c
}

func TestClientSendAndSubscribe(t *testing.T) {
	c := startHelperClient(t)
	events, unsub := c.Subscribe()
	defer unsub()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state, err := c.State(ctx)
	if err != nil {
		t.Fatalf("State failed: %v", err)
	}
	if state["sessionId"] != "sess-123" {
		t.Fatalf("unexpected sessionId: %#v", state["sessionId"])
	}

	stats, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if stats["turns"] != float64(1) {
		t.Fatalf("unexpected turns: %#v", stats["turns"])
	}

	res, err := c.Send(ctx, map[string]any{"type": "fail"})
	if err != nil {
		t.Fatalf("Send(fail) error: %v", err)
	}
	if res.Success || res.Error != "intentional failure" {
		t.Fatalf("unexpected fail response: %+v", res)
	}

	res, err = c.Send(ctx, map[string]any{"type": "prompt", "message": "hi"})
	if err != nil || !res.Success {
		t.Fatalf("Send(prompt) failed: res=%+v err=%v", res, err)
	}

	var gotUpdate, gotEnd bool
	for !(gotUpdate && gotEnd) {
		select {
		case ev := <-events:
			switch ev["type"] {
			case "message_update":
				gotUpdate = true
				if _, exists := ev["message"]; exists {
					t.Fatal("expected slimEvent to strip cumulative message")
				}
				ame := ev["assistantMessageEvent"].(map[string]any)
				if _, exists := ame["partial"]; exists {
					t.Fatal("expected slimEvent to strip partial")
				}
				block, ok := ame["block"].(map[string]any)
				if !ok || block["name"] != "read" {
					t.Fatalf("expected lifted toolCall block, got %#v", ame["block"])
				}
			case "agent_end":
				gotEnd = true
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for events (update=%v end=%v)", gotUpdate, gotEnd)
		}
	}

	if err := c.CloseStdin(); err != nil {
		t.Fatalf("CloseStdin: %v", err)
	}
	if err := c.Wait(); err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if _, err := c.Send(ctx, map[string]any{"type": "get_state"}); err == nil {
		t.Fatal("expected Send on exited client to fail")
	}
}
