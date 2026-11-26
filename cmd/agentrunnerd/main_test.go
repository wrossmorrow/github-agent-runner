package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wrossmorrow/agentrunner/api/v1alpha1"
	"github.com/wrossmorrow/agentrunner/pkg/review"
)

type stubExecutor struct {
	called  bool
	outcome *review.Outcome
	err     error
}

func (s *stubExecutor) Run(_ context.Context, _ review.Request, logOut io.Writer) (*review.Outcome, error) {
	s.called = true
	_, _ = fmt.Fprintln(logOut, "::group::Tool: read")
	_, _ = fmt.Fprintln(logOut, "::endgroup::")
	return s.outcome, s.err
}

func TestHealthAndReviewStreaming(t *testing.T) {
	mux := newMux(&stubExecutor{
		outcome: &review.Outcome{
			ExitCode: 1,
			Turns:    3,
			Result: &review.Result{
				Verdict: "REQUEST_CHANGES",
				Summary: "Missing tests",
				Comments: []review.Comment{
					{Path: "main.go", Line: 10, Severity: "error", Body: "Add test"},
				},
			},
		},
	}, v1alpha1.AgentModeReadOnly)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", rec.Code)
	}

	for _, path := range []string{"/review", "/run"} {
		body := strings.NewReader(`{"workspace":"/home/runner/_work/repo","instructions":"check tests"}`)
		req = httptest.NewRequest(http.MethodPost, path, body)
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", path, rec.Code, rec.Body.String())
		}
		out := rec.Body.String()
		if !strings.Contains(out, "::group::Tool: read") {
			t.Fatalf("missing streamed log group for %s in:\n%s", path, out)
		}

		lines := strings.Split(strings.TrimSpace(out), "\n")
		last := lines[len(lines)-1]
		if !strings.HasPrefix(last, "AGENTRUNNER_RESULT=") {
			t.Fatalf("expected AGENTRUNNER_RESULT trailer for %s, got %q", path, last)
		}

		var trailer resultTrailer
		if err := json.Unmarshal([]byte(strings.TrimPrefix(last, "AGENTRUNNER_RESULT=")), &trailer); err != nil {
			t.Fatalf("unmarshal trailer: %v", err)
		}
		if trailer.ExitCode != 1 || trailer.Verdict != "REQUEST_CHANGES" || trailer.Findings != 1 {
			t.Fatalf("unexpected trailer: %+v", trailer)
		}
	}
}

func TestUnsupportedModesReturn501(t *testing.T) {
	for _, mode := range []string{v1alpha1.AgentModeSuggest, v1alpha1.AgentModeWriteBranch} {
		t.Run(mode, func(t *testing.T) {
			exec := &stubExecutor{}
			mux := newMux(exec, mode)

			body := strings.NewReader(`{"workspace":"/home/runner/_work/repo","instructions":"check tests"}`)
			req := httptest.NewRequest(http.MethodPost, "/review", body)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusNotImplemented, rec.Body.String())
			}
			if exec.called {
				t.Fatal("executor should not be called for unimplemented mode")
			}
		})
	}
}

func TestOutputValidationOnRun(t *testing.T) {
	cases := []struct {
		output     string
		wantStatus int
	}{
		{review.OutputReleaseDraft, http.StatusNotImplemented},
		{review.OutputDeploymentStatus, http.StatusNotImplemented},
		{review.OutputBranchCommit, http.StatusNotImplemented},
		{review.OutputRevertPR, http.StatusNotImplemented},
		{"bogus-output", http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.output, func(t *testing.T) {
			exec := &stubExecutor{}
			mux := newMux(exec, v1alpha1.AgentModeReadOnly)

			body := strings.NewReader(fmt.Sprintf(`{"workspace":"/home/runner/_work/repo","instructions":"run task","output":%q}`, tc.output))
			req := httptest.NewRequest(http.MethodPost, "/run", body)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("output=%q status = %d, want %d: %s", tc.output, rec.Code, tc.wantStatus, rec.Body.String())
			}
			if exec.called {
				t.Fatalf("executor should not be called when output %q is rejected", tc.output)
			}
		})
	}
}

func TestLoadConfigModePrecedence(t *testing.T) {
	t.Run("missing file uses AGENTRUNNER_MODE", func(t *testing.T) {
		t.Setenv("AGENTRUNNER_MODE", v1alpha1.AgentModeSuggest)
		cfg, err := loadConfig(filepath.Join(t.TempDir(), "missing.json"))
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.Mode != v1alpha1.AgentModeSuggest {
			t.Fatalf("Mode = %q, want %q", cfg.Mode, v1alpha1.AgentModeSuggest)
		}
	})

	t.Run("file mode preserved when env unset", func(t *testing.T) {
		t.Setenv("AGENTRUNNER_MODE", "")
		cfgPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(cfgPath, []byte(`{"mode":"suggest"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.Mode != v1alpha1.AgentModeSuggest {
			t.Fatalf("Mode = %q, want %q", cfg.Mode, v1alpha1.AgentModeSuggest)
		}
	})

	t.Run("env overrides file mode", func(t *testing.T) {
		t.Setenv("AGENTRUNNER_MODE", v1alpha1.AgentModeWriteBranch)
		cfgPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(cfgPath, []byte(`{"mode":"read-only"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if cfg.Mode != v1alpha1.AgentModeWriteBranch {
			t.Fatalf("Mode = %q, want %q", cfg.Mode, v1alpha1.AgentModeWriteBranch)
		}
	})
}
