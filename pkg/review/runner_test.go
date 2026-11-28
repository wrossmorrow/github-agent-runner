package review

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wrossmorrow/github-agent-runner/api/v1alpha1"
)

func TestHelperPiReview(t *testing.T) {
	if os.Getenv("GO_WANT_REVIEW_HELPER") != "1" {
		return
	}
	mode := os.Getenv("HELPER_MODE")
	sc := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	promptCount := 0
	for sc.Scan() {
		var cmd map[string]any
		if err := json.Unmarshal(sc.Bytes(), &cmd); err != nil {
			continue
		}
		if cmd["type"] == "get_session_stats" {
			_ = enc.Encode(map[string]any{
				"type":    "response",
				"id":      cmd["id"],
				"success": true,
				"data":    map[string]any{"totalTokens": 42},
			})
			continue
		}
		if cmd["type"] != "prompt" {
			continue
		}
		promptCount++
		if mode == "pi_auto_retry_during_backoff" && promptCount > 1 {
			_ = enc.Encode(map[string]any{"type": "response", "id": cmd["id"], "success": false, "error": "unexpected duplicate prompt"})
			continue
		}
		if mode == "already_processing_then_followup" && promptCount > 1 {
			if cmd["streamingBehavior"] != "followUp" {
				_ = enc.Encode(map[string]any{"type": "response", "id": cmd["id"], "success": false, "error": "Agent is already processing"})
				continue
			}
			_ = enc.Encode(map[string]any{"type": "response", "id": cmd["id"], "success": true})
			_ = enc.Encode(map[string]any{"type": "turn_start"})
			_ = enc.Encode(map[string]any{
				"type": "agent_end",
				"messages": []any{
					map[string]any{
						"role":    "assistant",
						"content": `{"verdict":"APPROVE","summary":"Recovered via followUp","comments":[]}`,
					},
				},
			})
			continue
		}
		_ = enc.Encode(map[string]any{"type": "response", "id": cmd["id"], "success": true})

		if mode == "pi_auto_retry_during_backoff" && promptCount == 1 {
			_ = enc.Encode(map[string]any{"type": "turn_start"})
			_ = enc.Encode(map[string]any{
				"type": "agent_end",
				"messages": []any{
					map[string]any{
						"role":         "assistant",
						"stopReason":   "error",
						"errorMessage": "UND_ERR_SOCKET: other side closed",
						"content":      []any{},
					},
				},
			})
			time.Sleep(10 * time.Millisecond)
			_ = enc.Encode(map[string]any{"type": "auto_retry_start"})
			_ = enc.Encode(map[string]any{"type": "turn_start"})
			_ = enc.Encode(map[string]any{
				"type": "agent_end",
				"messages": []any{
					map[string]any{
						"role":    "assistant",
						"content": `{"verdict":"APPROVE","summary":"Pi auto-retried","comments":[]}`,
					},
				},
			})
			continue
		}

		if mode == "already_processing_then_followup" && promptCount == 1 {
			_ = enc.Encode(map[string]any{"type": "turn_start"})
			_ = enc.Encode(map[string]any{
				"type": "agent_end",
				"messages": []any{
					map[string]any{
						"role":         "assistant",
						"stopReason":   "error",
						"errorMessage": "UND_ERR_SOCKET: other side closed",
						"content":      []any{},
					},
				},
			})
			continue
		}

		if mode == "too_many_turns" {
			for i := 0; i < 5; i++ {
				_ = enc.Encode(map[string]any{"type": "turn_start"})
			}
			continue
		}

		if mode == "model_error" {
			_ = enc.Encode(map[string]any{
				"type": "agent_end",
				"messages": []any{
					map[string]any{
						"role":         "assistant",
						"stopReason":   "error",
						"errorMessage": "401 Unauthorized",
						"content":      []any{},
					},
				},
			})
			continue
		}

		if mode == "transient_socket_then_ok" && promptCount == 1 {
			_ = enc.Encode(map[string]any{"type": "turn_start"})
			_ = enc.Encode(map[string]any{
				"type": "agent_end",
				"messages": []any{
					map[string]any{
						"role":         "assistant",
						"stopReason":   "error",
						"errorMessage": "UND_ERR_SOCKET: other side closed",
						"content":      []any{},
					},
				},
			})
			continue
		}

		if mode == "bad_format_then_ok" && promptCount == 1 {
			_ = enc.Encode(map[string]any{"type": "turn_start"})
			_ = enc.Encode(map[string]any{
				"type": "agent_end",
				"messages": []any{
					map[string]any{
						"role":    "assistant",
						"content": "Everything looks great, LGTM!",
					},
				},
			})
			continue
		}

		if mode == "release_markdown" {
			_ = enc.Encode(map[string]any{"type": "turn_start"})
			_ = enc.Encode(map[string]any{
				"type": "agent_end",
				"messages": []any{
					map[string]any{
						"role":    "assistant",
						"content": "# v0.1.0\n\n## Features\n- Added `release-draft`",
					},
				},
			})
			continue
		}

		if (mode == "transient_socket_then_ok" || mode == "bad_format_then_ok") && promptCount > 1 {
			if sp, _ := cmd["systemPrompt"].(string); strings.TrimSpace(sp) == "" {
				_ = enc.Encode(map[string]any{
					"type": "agent_end",
					"messages": []any{
						map[string]any{
							"role":         "assistant",
							"stopReason":   "error",
							"errorMessage": "missing systemPrompt on follow-up",
							"content":      []any{},
						},
					},
				})
				continue
			}
			summary := "Recovered cleanly"
			if mode == "bad_format_then_ok" {
				summary = "Formatted after nudge"
			}
			_ = enc.Encode(map[string]any{"type": "turn_start"})
			_ = enc.Encode(map[string]any{
				"type": "agent_end",
				"messages": []any{
					map[string]any{
						"role":    "assistant",
						"content": `{"verdict":"APPROVE","summary":"` + summary + `","comments":[]}`,
					},
				},
			})
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
	if outcome.SessionID == "" {
		t.Fatal("expected generated SessionID on Outcome")
	}
	if !poster.called || poster.req.PRNumber != 42 || poster.req.Repo != "org/repo" || poster.req.SessionID != outcome.SessionID {
		t.Fatalf("poster not called with enriched PR info & SessionID: %+v", poster)
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

func TestWritePiConfigDir(t *testing.T) {
	dir, err := writePiConfigDir(Config{
		Model:           "flint",
		GatewayEndpoint: "http://host.docker.internal:11500/v1",
		Limits: v1alpha1.LimitsSpec{
			ContextWindow: 262144,
			MaxTokens:     32768,
		},
		Attribution: map[string]string{
			"sessionName": "x-custom-session-name",
		},
	}, Request{
		Repo:     "my-org/my-repo",
		RunID:    "999",
		PRNumber: 17,
	}, "base instructions", "secret-tok\n", "sess-uuid-42", "suggest", "my-org/my-repo#17")
	if err != nil {
		t.Fatalf("writePiConfigDir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	agentsRaw, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil || strings.TrimSpace(string(agentsRaw)) != "base instructions" {
		t.Fatalf("unexpected AGENTS.md: %q err=%v", string(agentsRaw), err)
	}

	modelsRaw, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatalf("reading models.json: %v", err)
	}
	var parsed piModelsFile
	if err := json.Unmarshal(modelsRaw, &parsed); err != nil {
		t.Fatalf("unmarshaling models.json: %v", err)
	}
	gw, ok := parsed.Providers["gateway"]
	if !ok {
		t.Fatalf("expected gateway provider, got %+v", parsed.Providers)
	}
	if gw.BaseURL != "http://host.docker.internal:11500/v1" || gw.API != "openai-completions" || gw.APIKey != "secret-tok" || !gw.AuthHeader {
		t.Fatalf("unexpected provider config: %+v", gw)
	}
	if gw.Headers["Authorization"] != "Bearer secret-tok" {
		t.Fatalf("unexpected Authorization header: %q", gw.Headers["Authorization"])
	}
	if gw.Headers["x-github-repo"] != "my-org/my-repo" || gw.Headers["x-github-pr"] != "17" ||
		gw.Headers["x-session-id"] != "sess-uuid-42" || gw.Headers["x-session-kind"] != "suggest" ||
		gw.Headers["x-custom-session-name"] != "my-org/my-repo#17" {
		t.Fatalf("unexpected attribution headers: %+v", gw.Headers)
	}
	if len(gw.Models) != 1 || gw.Models[0].ID != "flint" || gw.Models[0].Compat.MaxTokensField != "max_tokens" {
		t.Fatalf("unexpected model config: %+v", gw.Models)
	}
	if gw.Models[0].ContextWindow != 262144 || gw.Models[0].MaxTokens != 32768 {
		t.Fatalf("unexpected token limits in model config: %+v", gw.Models[0])
	}
}

func TestRunnerModelErrorSurfaced(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_WANT_REVIEW_HELPER", "1")
	t.Setenv("HELPER_MODE", "model_error")

	runner := &Runner{
		Config: Config{
			PiBin:  exe,
			PiArgs: []string{"-test.run=TestHelperPiReview"},
			Limits: v1alpha1.LimitsSpec{MaxTurns: 5, MaxDuration: "5s"},
		},
	}

	outcome, err := runner.Run(context.Background(), Request{
		Workspace:    t.TempDir(),
		Instructions: "Check everything",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Fatalf("expected surfaced model error, got %v", err)
	}
	if outcome == nil || outcome.SessionID == "" {
		t.Fatalf("expected non-empty SessionID on error outcome, got %+v", outcome)
	}
}

func TestRunnerTransientTransportRetry(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_WANT_REVIEW_HELPER", "1")
	t.Setenv("HELPER_MODE", "transient_socket_then_ok")

	poster := &mockPoster{}
	runner := &Runner{
		Config: Config{
			PiBin:  exe,
			PiArgs: []string{"-test.run=TestHelperPiReview"},
			Limits: v1alpha1.LimitsSpec{MaxTurns: 5, MaxDuration: "5s"},
		},
		Poster:       poster,
		RetryBackoff: time.Millisecond,
	}

	var logs bytes.Buffer
	outcome, err := runner.Run(context.Background(), Request{
		Workspace:    t.TempDir(),
		Instructions: "Check everything",
		Output:       "pr-review",
		Repo:         "org/repo",
		PRNumber:     3,
	}, &logs)
	if err != nil {
		t.Fatalf("expected transient transport retry to recover, got %v", err)
	}
	if outcome.Result.Verdict != "APPROVE" || outcome.ExitCode != 0 || outcome.Turns != 2 {
		t.Fatalf("unexpected outcome after retry: %+v", outcome)
	}
	if !poster.called {
		t.Fatal("expected poster to be called after recovery")
	}
	if !strings.Contains(logs.String(), "retrying in-session (1/2)") {
		t.Fatalf("expected retry log line, got:\n%s", logs.String())
	}

	t.Run("disabled when MaxTransportRetries is zero", func(t *testing.T) {
		zero := 0
		noRetryRunner := &Runner{
			Config: Config{
				PiBin:  exe,
				PiArgs: []string{"-test.run=TestHelperPiReview"},
				Limits: v1alpha1.LimitsSpec{
					MaxTurns:            5,
					MaxDuration:         "5s",
					MaxTransportRetries: &zero,
				},
			},
			RetryBackoff: time.Millisecond,
		}
		_, err := noRetryRunner.Run(context.Background(), Request{
			Workspace:    t.TempDir(),
			Instructions: "Check everything",
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "UND_ERR_SOCKET") {
			t.Fatalf("expected immediate UND_ERR_SOCKET error when retries=0, got %v", err)
		}
	})
}

func TestRunnerFormatNudge(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_WANT_REVIEW_HELPER", "1")
	t.Setenv("HELPER_MODE", "bad_format_then_ok")

	poster := &mockPoster{}
	runner := &Runner{
		Config: Config{
			PiBin:  exe,
			PiArgs: []string{"-test.run=TestHelperPiReview"},
			Limits: v1alpha1.LimitsSpec{MaxTurns: 5, MaxDuration: "5s"},
		},
		Poster: poster,
	}

	var logs bytes.Buffer
	outcome, err := runner.Run(context.Background(), Request{
		Workspace:    t.TempDir(),
		Instructions: "Check everything",
		Output:       "pr-review",
		Repo:         "org/repo",
		PRNumber:     9,
	}, &logs)
	if err != nil {
		t.Fatalf("expected format nudge to recover, got %v", err)
	}
	if outcome.Result.Verdict != "APPROVE" || outcome.Result.Summary != "Formatted after nudge" || outcome.Turns != 2 {
		t.Fatalf("unexpected outcome after nudge: %+v", outcome)
	}
	if !poster.called {
		t.Fatal("expected poster to be called after format nudge")
	}
	if !strings.Contains(logs.String(), "nudging once in-session") {
		t.Fatalf("expected nudge log line, got:\n%s", logs.String())
	}

	t.Run("respects MaxTurns budget", func(t *testing.T) {
		budgetRunner := &Runner{
			Config: Config{
				PiBin:  exe,
				PiArgs: []string{"-test.run=TestHelperPiReview"},
				Limits: v1alpha1.LimitsSpec{MaxTurns: 1, MaxDuration: "5s"},
			},
		}
		_, err := budgetRunner.Run(context.Background(), Request{
			Workspace:    t.TempDir(),
			Instructions: "Check everything",
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "no valid review result JSON") {
			t.Fatalf("expected JSON parse error without exceeding MaxTurns=1, got %v", err)
		}
	})
}

func TestRunnerTransportRetryRaceRecovery(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		helperMode  string
		backoff     time.Duration
		wantSummary string
		wantLog     string
	}{
		{
			name:        "awaits pi internal auto_retry_start during backoff",
			helperMode:  "pi_auto_retry_during_backoff",
			backoff:     500 * time.Millisecond,
			wantSummary: "Pi auto-retried",
			wantLog:     "pi became active during backoff (auto_retry_start)",
		},
		{
			name:        "falls back to streamingBehavior=followUp when pi still processing",
			helperMode:  "already_processing_then_followup",
			backoff:     time.Millisecond,
			wantSummary: "Recovered via followUp",
			wantLog:     "streamingBehavior=followUp",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GO_WANT_REVIEW_HELPER", "1")
			t.Setenv("HELPER_MODE", tc.helperMode)

			poster := &mockPoster{}
			runner := &Runner{
				Config: Config{
					PiBin:  exe,
					PiArgs: []string{"-test.run=TestHelperPiReview"},
					Limits: v1alpha1.LimitsSpec{MaxTurns: 5, MaxDuration: "5s"},
				},
				Poster:       poster,
				RetryBackoff: tc.backoff,
			}

			var logs bytes.Buffer
			outcome, err := runner.Run(context.Background(), Request{
				Workspace:    t.TempDir(),
				Instructions: "Check everything",
				Output:       "pr-review",
				Repo:         "org/repo",
				PRNumber:     11,
			}, &logs)
			if err != nil {
				t.Fatalf("Run(%s) failed: %v\nlogs:\n%s", tc.helperMode, err, logs.String())
			}
			if outcome.Result.Verdict != "APPROVE" || outcome.Result.Summary != tc.wantSummary {
				t.Fatalf("unexpected outcome: %+v", outcome.Result)
			}
			if !poster.called {
				t.Fatal("expected poster to be called after recovery")
			}
			if !strings.Contains(logs.String(), tc.wantLog) {
				t.Fatalf("expected log substring %q, got:\n%s", tc.wantLog, logs.String())
			}
		})
	}
}

func TestRunnerReleaseDraftMarkdown(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_WANT_REVIEW_HELPER", "1")
	t.Setenv("HELPER_MODE", "release_markdown")

	poster := &mockPoster{}
	runner := &Runner{
		Config: Config{
			PiBin:  exe,
			PiArgs: []string{"-test.run=TestHelperPiReview"},
			Limits: v1alpha1.LimitsSpec{MaxTurns: 5, MaxDuration: "5s"},
		},
		Poster: poster,
	}

	outcome, err := runner.Run(context.Background(), Request{
		Workspace:    t.TempDir(),
		Instructions: "Draft release notes for v0.1.0",
		Output:       OutputReleaseDraft,
		Repo:         "org/repo",
		Tag:          "v0.1.0",
	}, nil)
	if err != nil {
		t.Fatalf("Run release-draft failed: %v", err)
	}
	if outcome.Result.Title != "v0.1.0" || !strings.Contains(outcome.Result.Body, "Added `release-draft`") {
		t.Fatalf("unexpected release result: %+v", outcome.Result)
	}
	if !poster.called || poster.req.Output != OutputReleaseDraft || poster.req.Tag != "v0.1.0" {
		t.Fatalf("expected poster called with release-draft request, got %+v", poster)
	}
}

func TestResolvePreviousTag(t *testing.T) {
	runGit := func(t *testing.T, dir string, args ...string) {
		t.Helper()
		full := append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}

	t.Run("no prior tag returns empty string", func(t *testing.T) {
		repo := t.TempDir()
		runGit(t, repo, "init")
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("init\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo, "add", "README.md")
		runGit(t, repo, "commit", "-m", "initial commit")
		runGit(t, repo, "tag", "v0.1.0")

		if got := resolvePreviousTag(context.Background(), repo, "v0.1.0"); got != "" {
			t.Fatalf("expected empty previous tag, got %q", got)
		}
	})

	t.Run("annotated and lightweight tags resolve previous tag", func(t *testing.T) {
		repo := t.TempDir()
		runGit(t, repo, "init")
		if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("one\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo, "add", "file.txt")
		runGit(t, repo, "commit", "-m", "first")
		runGit(t, repo, "tag", "v0.1.0") // lightweight

		if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("two\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo, "add", "file.txt")
		runGit(t, repo, "commit", "-m", "second")
		runGit(t, repo, "tag", "-a", "v0.2.0", "-m", "release v0.2.0") // annotated

		if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("three\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo, "add", "file.txt")
		runGit(t, repo, "commit", "-m", "third")
		runGit(t, repo, "tag", "v0.3.0") // lightweight

		if got := resolvePreviousTag(context.Background(), repo, "v0.2.0"); got != "v0.1.0" {
			t.Fatalf("resolvePreviousTag(annotated v0.2.0) = %q, want v0.1.0", got)
		}
		if got := resolvePreviousTag(context.Background(), repo, "v0.3.0"); got != "v0.2.0" {
			t.Fatalf("resolvePreviousTag(lightweight v0.3.0) = %q, want v0.2.0", got)
		}
		if got := resolvePreviousTag(context.Background(), repo, ""); got != "v0.2.0" {
			t.Fatalf("resolvePreviousTag(HEAD fallback) = %q, want v0.2.0", got)
		}
		if got := resolvePreviousTag(context.Background(), repo, "--help"); got != "v0.2.0" {
			t.Fatalf("resolvePreviousTag(--help fallback) = %q, want v0.2.0", got)
		}
	})
}

func TestSelectDefaultSystemPrompt(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		output string
		want   string
	}{
		{"default reviewer", v1alpha1.AgentModeReadOnly, OutputPRReview, DefaultReviewerSystemPrompt},
		{"mode suggest", v1alpha1.AgentModeSuggest, OutputPRReview, DefaultSuggestSystemPrompt},
		{"output suggest", v1alpha1.AgentModeReadOnly, OutputSuggest, DefaultSuggestSystemPrompt},
		{"mode write-branch", v1alpha1.AgentModeWriteBranch, OutputBranchCommit, DefaultWriteBranchSystemPrompt},
		{"write-branch non-write output keeps reviewer prompt", v1alpha1.AgentModeWriteBranch, OutputStdout, DefaultReviewerSystemPrompt},
		{"write-branch suggest output keeps suggest prompt", v1alpha1.AgentModeWriteBranch, OutputSuggest, DefaultSuggestSystemPrompt},
		{"output revert-pr", v1alpha1.AgentModeWriteBranch, OutputRevertPR, DefaultWriteBranchSystemPrompt},
		{"release draft takes precedence", v1alpha1.AgentModeSuggest, OutputReleaseDraft, DefaultReleaseNotesSystemPrompt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectDefaultSystemPrompt(tc.mode, tc.output); got != tc.want {
				t.Fatalf("selectDefaultSystemPrompt(%q, %q) did not match expected prompt", tc.mode, tc.output)
			}
		})
	}
}

func TestRunnerWriteOutputRejectsDirtyWorkspace(t *testing.T) {
	_, repo := initTestRepoWithBareRemote(t)

	// Leave an untracked file in the workspace before running a write output.
	if err := os.WriteFile(filepath.Join(repo, "stray-artifact.tmp"), []byte("leftover\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := &Runner{
		Config: Config{
			PiBin: filepath.Join(t.TempDir(), "nonexistent-pi"),
			Mode:  v1alpha1.AgentModeWriteBranch,
		},
	}

	_, err := runner.Run(context.Background(), Request{
		Workspace:    repo,
		Instructions: "Fix tests",
		Output:       OutputBranchCommit,
		TargetBranch: "bot/fix-dirty",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "pre-existing uncommitted changes") {
		t.Fatalf("expected dirty workspace error before starting pi, got %v", err)
	}

	_, nonGitErr := runner.Run(context.Background(), Request{
		Workspace:    t.TempDir(),
		Instructions: "Fix tests",
		Output:       OutputBranchCommit,
		TargetBranch: "bot/fix-nongit",
	}, nil)
	if nonGitErr == nil || !strings.Contains(nonGitErr.Error(), "checking workspace git status") {
		t.Fatalf("expected git status failure before starting pi on non-git workspace, got %v", nonGitErr)
	}
}

func TestBuildChildEnvStripsSidecarSecrets(t *testing.T) {
	t.Setenv("SESSION_SINK_TOKEN", "sink-top-secret")
	t.Setenv("AGENT_GATEWAY_TOKEN", "gateway-top-secret")
	t.Setenv("AGENTRUNNER_MCP_TOKEN_INTERNAL_DOCS", "mcp-top-secret")

	env := buildChildEnv(Config{Model: "gpt-test", Provider: "gateway"}, Request{Repo: "org/repo", RunID: "123"}, "/tmp/pi-dir")
	for _, entry := range env {
		if strings.HasPrefix(entry, "SESSION_SINK_TOKEN=") ||
			strings.HasPrefix(entry, "AGENT_GATEWAY_TOKEN=") ||
			strings.HasPrefix(entry, "AGENTRUNNER_MCP_TOKEN_") {
			t.Fatalf("child env leaked sidecar secret %q", entry)
		}
	}
	foundDir := false
	foundRepo := false
	for _, entry := range env {
		if entry == "PI_CODING_AGENT_DIR=/tmp/pi-dir" {
			foundDir = true
		}
		if entry == "AGENT_GITHUB_REPO=org/repo" {
			foundRepo = true
		}
	}
	if !foundDir || !foundRepo {
		t.Fatalf("expected PI_CODING_AGENT_DIR and AGENT_GITHUB_REPO in child env, got %v", env)
	}
}

func TestWritePiConfigDirWritesMCPJSON(t *testing.T) {
	t.Setenv("AGENTRUNNER_MCP_TOKEN_INTERNAL_DOCS", "mcp-secret-tok")

	dir, err := writePiConfigDir(Config{
		MCPServers: []MCPServerConfig{
			{
				Name:         "internal-docs",
				URL:          "http://docs-mcp.platform.svc.cluster.local:8080/mcp",
				Headers:      map[string]string{"X-Org": "example"},
				AuthTokenEnv: "AGENTRUNNER_MCP_TOKEN_INTERNAL_DOCS",
			},
			{
				Name:    "local-tool",
				Command: "/usr/local/bin/local-tool",
				Args:    []string{"--stdio"},
			},
		},
	}, Request{}, "", "", "", "", "")
	if err != nil {
		t.Fatalf("writePiConfigDir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	raw, err := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if err != nil {
		t.Fatalf("reading mcp.json: %v", err)
	}
	var parsed struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshaling mcp.json: %v", err)
	}
	docs, ok := parsed.MCPServers["internal-docs"]
	if !ok || docs.URL != "http://docs-mcp.platform.svc.cluster.local:8080/mcp" ||
		docs.Headers["Authorization"] != "Bearer mcp-secret-tok" || docs.Headers["X-Org"] != "example" {
		t.Fatalf("unexpected remote HTTP mcp.json entry: %+v", docs)
	}
	local, ok := parsed.MCPServers["local-tool"]
	if !ok || local.Command != "/usr/local/bin/local-tool" || len(local.Args) != 1 || local.Args[0] != "--stdio" {
		t.Fatalf("unexpected stdio mcp.json entry: %+v", local)
	}
}
