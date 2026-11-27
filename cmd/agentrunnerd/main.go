// Command agentrunnerd runs inside the `agent` native sidecar of an ARC v2
// runner pod, accepting POST /run (and alias POST /review) requests from the stock runner step.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wrossmorrow/agentrunner/api/v1alpha1"
	"github.com/wrossmorrow/agentrunner/pkg/review"
)

const defaultAddr = "127.0.0.1:8788"

type reviewExecutor interface {
	Run(ctx context.Context, req review.Request, logOut io.Writer) (*review.Outcome, error)
}

func main() {
	addr := envOr("AGENTRUNNER_ADDR", defaultAddr)
	cfgPath := envOr("AGENTRUNNER_CONFIG", "/etc/agentrunner/config.json")
	tokenFile := envOr("AGENTRUNNER_GH_TOKEN_FILE", "/tokens/token")

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	var protectedBranches []string
	if cfg.ProtectedBranches != nil {
		protectedBranches = *cfg.ProtectedBranches
	}

	runner := &review.Runner{
		Config: cfg,
		Poster: &review.GhPoster{
			TokenFile:         tokenFile,
			ProtectedBranches: protectedBranches,
		},
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(runner, cfg.Mode),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("agentrunnerd listening on %s (model=%s mode=%s)", addr, cfg.Model, cfg.Mode)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server failed: %v", err)
	}
}

func loadConfig(path string) (review.Config, error) {
	cfg := review.Config{
		Harness:    "pi",
		Mode:       v1alpha1.AgentModeReadOnly,
		AgentsDirs: []string{"/opt/agents", "/etc/agentrunner/prompts", "/etc/agentrunner"},
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		applyConfigEnvOverrides(&cfg)
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Mode == "" {
		cfg.Mode = v1alpha1.AgentModeReadOnly
	}
	if len(cfg.AgentsDirs) == 0 {
		cfg.AgentsDirs = []string{"/opt/agents", "/etc/agentrunner/prompts", "/etc/agentrunner"}
	}
	applyConfigEnvOverrides(&cfg)
	return cfg, nil
}

func applyConfigEnvOverrides(cfg *review.Config) {
	if envMode := os.Getenv("AGENTRUNNER_MODE"); envMode != "" {
		cfg.Mode = envMode
	}
	if rawBranches := os.Getenv("AGENTRUNNER_PROTECTED_BRANCHES"); rawBranches != "" {
		var branches []string
		for _, part := range strings.Split(rawBranches, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				branches = append(branches, trimmed)
			}
		}
		if len(branches) > 0 {
			cfg.ProtectedBranches = &branches
		}
	}
}

func newMux(exec reviewExecutor, mode string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	handler := handleRun(exec, mode)
	mux.HandleFunc("POST /run", handler)
	mux.HandleFunc("POST /review", handler)
	return mux
}

type resultTrailer struct {
	SessionID string `json:"sessionId,omitempty"`
	ExitCode  int    `json:"exitCode"`
	Verdict   string `json:"verdict,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Findings  int    `json:"findings"`
	Turns     int    `json:"turns"`
	Error     string `json:"error,omitempty"`
}

func handleRun(exec reviewExecutor, mode string) http.HandlerFunc {
	effectiveMode := mode
	if effectiveMode == "" {
		effectiveMode = v1alpha1.AgentModeReadOnly
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch effectiveMode {
		case v1alpha1.AgentModeReadOnly, v1alpha1.AgentModeSuggest, v1alpha1.AgentModeWriteBranch:
		default:
			http.Error(w, fmt.Sprintf("agent mode %q is not yet implemented", effectiveMode), http.StatusNotImplemented)
			return
		}
		var req review.Request
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON request: %v", err), http.StatusBadRequest)
			return
		}
		if req.Workspace == "" || req.Instructions == "" {
			http.Error(w, "workspace and instructions are required", http.StatusBadRequest)
			return
		}
		if err := review.CheckOutputSupport(req.Output); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, review.ErrOutputNotImplemented) {
				status = http.StatusNotImplemented
			}
			http.Error(w, err.Error(), status)
			return
		}
		if review.IsWriteOutput(req.Output) && effectiveMode != v1alpha1.AgentModeWriteBranch {
			http.Error(w, fmt.Sprintf("output %q requires agent mode %q (current mode: %q)", req.Output, v1alpha1.AgentModeWriteBranch, effectiveMode), http.StatusForbidden)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)

		fw := &flushWriter{w: w}
		if flusher, ok := w.(http.Flusher); ok {
			fw.flusher = flusher
		}

		outcome, err := exec.Run(r.Context(), req, fw)
		var trailer resultTrailer
		if err != nil {
			trailer = resultTrailer{ExitCode: 1, Error: err.Error()}
			if outcome != nil {
				trailer.SessionID = outcome.SessionID
				trailer.Turns = outcome.Turns
			}
			_, _ = fmt.Fprintf(fw, "::error::%s\n", err.Error())
		} else {
			trailer = resultTrailer{
				SessionID: outcome.SessionID,
				ExitCode:  outcome.ExitCode,
				Verdict:   outcome.Result.Verdict,
				Summary:   outcome.Result.Summary,
				Findings:  len(outcome.Result.Comments),
				Turns:     outcome.Turns,
			}
			_, _ = fmt.Fprintf(fw, "\nSummary (%s): %s\n", outcome.Result.Verdict, outcome.Result.Summary)
		}

		b, _ := json.Marshal(trailer)
		_, _ = fmt.Fprintf(fw, "AGENTRUNNER_RESULT=%s\n", string(b))
	}
}

type flushWriter struct {
	mu      sync.Mutex
	w       io.Writer
	flusher http.Flusher
}

func (f *flushWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.w.Write(p)
	if f.flusher != nil {
		f.flusher.Flush()
	}
	return n, err
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
