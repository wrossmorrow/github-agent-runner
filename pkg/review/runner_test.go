package review

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wrossmorrow/agentrunner/api/v1alpha1"
)

func TestHelperPiReview(t *testing.T) {
	if os.Getenv("GO_WANT_REVIEW_HELPER") != "1" {
		return
	}
	mode := os.Getenv("HELPER_MODE")
	sc := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var cmd map[string]any
		if err := json.Unmarshal(sc.Bytes(), &cmd); err != nil {
			continue
		}
		if cmd["type"] != "prompt" {
			continue
		}
		_ = enc.Encode(map[string]any{"type": "response", "id": cmd["id"], "success": true})

		if mode == "too_many_turns" {
			for i := 0; i < 5; i++ {
				_ = enc.Encode(map[string]any{"type": "turn_start"})
			}
			continue
		}

		_ = enc.Encode(map[string]any{"type": "turn_start"})
		_ = enc.Encode(map[string]any{"type": "tool_execution_start", "toolName": "read"})
		_ = enc.Encode(map[string]any{"type": "tool_execution_end", "result": map[string]any{"lines": 20}})
		_ = enc.Encode(map[string]any{
			"type": "agent_end",
			"messages": []any{
				map[string]any{
					"role": "assistant",
					"content": "Here is my review:\n```json\n" +
						`{"verdict":"REQUEST_CHANGES","summary":"Missing unit tests","comments":[{"path":"pkg/a.go","line":12,"severity":"error","body":"Add test"}]}` +
						"\n```",
				},
			},
		})
	}
	os.Exit(0)
}

type mockPoster struct {
	called bool
	req    Request
	res    *Result
}

func (m *mockPoster) PostReview(_ context.Context, req Request, res *Result) error {
	m.called = true
	m.req = req
	m.res = res
	return nil
}

func TestAssembleAgentsMDOrder(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	_ = os.WriteFile(filepath.Join(d1, "15-toolchain.md"), []byte("toolchain"), 0o600)
	_ = os.WriteFile(filepath.Join(d2, "00-base.md"), []byte("base"), 0o600)
	_ = os.WriteFile(filepath.Join(d2, "20-deploy.md"), []byte("deploy"), 0o600)

	got, err := AssembleAgentsMD(d1, d2, filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("AssembleAgentsMD: %v", err)
	}
	want := "base\n\ntoolchain\n\ndeploy"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRunnerEndToEnd(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_WANT_REVIEW_HELPER", "1")
	t.Setenv("HELPER_MODE", "ok")

	ws := t.TempDir()
	_ = os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("repo rules"), 0o600)
	eventPath := filepath.Join(ws, "event.json")
	_ = os.WriteFile(eventPath, []byte(`{"pull_request":{"number":42,"base":{"ref":"main"},"head":{"sha":"abc123"}},"repository":{"full_name":"org/repo"}}`), 0o600)

	poster := &mockPoster{}
	runner := &Runner{
		Config: Config{
			PiBin:           exe,
			PiArgs:          []string{"-test.run=TestHelperPiReview"},
			Model:           "claude-sonnet-4-5",
			GatewayEndpoint: "https://gateway.internal/v1",
			Limits:          v1alpha1.LimitsSpec{MaxTurns: 5, MaxDuration: "10s"},
		},
		Poster: poster,
	}

	var logs bytes.Buffer
	outcome, err := runner.Run(context.Background(), Request{
		Workspace:    ws,
		EventPath:    eventPath,
		Instructions: "Ensure new functions have tests",
		FailOn:       "error",
		Output:       "pr-review",
	}, &logs)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if outcome.ExitCode != 1 || outcome.Result.Verdict != "REQUEST_CHANGES" {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	if !poster.called || poster.req.PRNumber != 42 || poster.req.Repo != "org/repo" {
		t.Fatalf("poster not called with enriched PR info: %+v", poster)
	}
	if !strings.Contains(logs.String(), "::group::Tool: read") || !strings.Contains(logs.String(), "::endgroup::") {
		t.Fatalf("expected ::group:: logs, got:\n%s", logs.String())
	}
}

func TestRunnerMaxTurnsExceeded(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_WANT_REVIEW_HELPER", "1")
	t.Setenv("HELPER_MODE", "too_many_turns")

	runner := &Runner{
		Config: Config{
			PiBin:  exe,
			PiArgs: []string{"-test.run=TestHelperPiReview"},
			Limits: v1alpha1.LimitsSpec{MaxTurns: 2, MaxDuration: (5 * time.Second).String()},
		},
	}

	_, err = runner.Run(context.Background(), Request{
		Workspace:    t.TempDir(),
		Instructions: "Check everything",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "maxTurns") {
		t.Fatalf("expected maxTurns error, got %v", err)
	}
}
