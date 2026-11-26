package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

	body := strings.NewReader(`{"workspace":"/home/runner/_work/repo","instructions":"check tests"}`)
	req = httptest.NewRequest(http.MethodPost, "/review", body)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("review status = %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "::group::Tool: read") {
		t.Fatalf("missing streamed log group in:\n%s", out)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "AGENTRUNNER_RESULT=") {
		t.Fatalf("expected AGENTRUNNER_RESULT trailer, got %q", last)
	}

	var trailer resultTrailer
	if err := json.Unmarshal([]byte(strings.TrimPrefix(last, "AGENTRUNNER_RESULT=")), &trailer); err != nil {
		t.Fatalf("unmarshal trailer: %v", err)
	}
	if trailer.ExitCode != 1 || trailer.Verdict != "REQUEST_CHANGES" || trailer.Findings != 1 {
		t.Fatalf("unexpected trailer: %+v", trailer)
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
