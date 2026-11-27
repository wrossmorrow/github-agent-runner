package review

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type handlerRoundTripper struct {
	handler http.Handler
}

func (rt handlerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rt.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

func TestUploadSessionArchive(t *testing.T) {
	sessionFile := filepath.Join(t.TempDir(), "abc.jsonl")
	wantTranscript := "{\"type\":\"turn_start\"}\n{\"type\":\"turn_end\"}\n"
	if err := os.WriteFile(sessionFile, []byte(wantTranscript), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotAuth string
	var gotSessionHeader string
	var gotMeta SessionArchiveMetadata
	var gotTranscript string

	client := &http.Client{
		Transport: handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotSessionHeader = r.Header.Get("X-Session-Id")
			mr, err := r.MultipartReader()
			if err != nil {
				t.Errorf("MultipartReader: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Errorf("NextPart: %v", err)
					break
				}
				switch part.FormName() {
				case "metadata":
					if err := json.NewDecoder(part).Decode(&gotMeta); err != nil {
						t.Errorf("decode metadata: %v", err)
					}
				case "transcript":
					gz, err := gzip.NewReader(part)
					if err != nil {
						t.Errorf("gzip.NewReader: %v", err)
						continue
					}
					raw, err := io.ReadAll(gz)
					_ = gz.Close()
					if err != nil {
						t.Errorf("read gzipped transcript: %v", err)
					}
					gotTranscript = string(raw)
				}
			}
			w.WriteHeader(http.StatusAccepted)
		})},
	}

	meta := SessionArchiveMetadata{
		SessionID:   "uuid-1234",
		SessionKind: "suggest",
		SessionName: "org/repo#17",
		Repo:        "org/repo",
		PRNumber:    17,
		Model:       "gpt-test",
		Verdict:     "APPROVE",
		Findings:    1,
		Turns:       2,
		EndReason:   "closed",
		StartedAt:   time.Now().UTC().Add(-time.Second),
		EndedAt:     time.Now().UTC(),
	}
	if err := uploadSessionArchive(context.Background(), client, "http://session-hub.local/v1/sessions", "sink-secret", meta, sessionFile); err != nil {
		t.Fatalf("uploadSessionArchive failed: %v", err)
	}

	if gotAuth != "Bearer sink-secret" {
		t.Fatalf("Authorization = %q, want Bearer sink-secret", gotAuth)
	}
	if gotSessionHeader != "uuid-1234" {
		t.Fatalf("X-Session-Id = %q, want uuid-1234", gotSessionHeader)
	}
	if gotMeta.SessionID != "uuid-1234" || gotMeta.SessionKind != "suggest" || gotMeta.SessionName != "org/repo#17" {
		t.Fatalf("unexpected uploaded metadata: %+v", gotMeta)
	}
	if gotTranscript != wantTranscript {
		t.Fatalf("decoded transcript = %q, want %q", gotTranscript, wantTranscript)
	}
}

func TestRunnerSessionArchiveFailOpen(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_WANT_REVIEW_HELPER", "1")
	t.Setenv("HELPER_MODE", "")

	var hits int
	var gotStats map[string]any
	var mu sync.Mutex
	sinkClient := &http.Client{
		Transport: handlerRoundTripper{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var meta SessionArchiveMetadata
			if mr, err := r.MultipartReader(); err == nil {
				for {
					part, err := mr.NextPart()
					if err != nil {
						break
					}
					if part.FormName() == "metadata" {
						_ = json.NewDecoder(part).Decode(&meta)
					}
				}
			}
			mu.Lock()
			hits++
			gotStats = meta.Stats
			mu.Unlock()
			http.Error(w, "sink unavailable", http.StatusInternalServerError)
		})},
	}

	runner := &Runner{
		Config: Config{
			PiBin:               exe,
			PiArgs:              []string{"-test.run=TestHelperPiReview"},
			SessionSinkEndpoint: "http://session-hub.local/v1/sessions",
		},
		SessionSinkClient: sinkClient,
	}

	var logs safeBuffer
	outcome, err := runner.Run(context.Background(), Request{
		Workspace:    t.TempDir(),
		Repo:         "org/repo",
		PRNumber:     7,
		Instructions: "Check tests",
	}, &logs)
	if err != nil {
		t.Fatalf("expected fail-open upload failure not to fail run, got %v", err)
	}
	if outcome.SessionID == "" {
		t.Fatal("expected generated SessionID on Outcome")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("expected 1 upload attempt to sessionSink, got %d", hits)
	}
	if gotStats == nil || gotStats["totalTokens"] != float64(42) {
		t.Fatalf("expected session stats from pi in archive metadata, got %+v", gotStats)
	}
	if !strings.Contains(logs.String(), "session archive upload failed") {
		t.Fatalf("expected archive failure warning in logs, got:\n%s", logs.String())
	}
}

type safeBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestResolveSessionID(t *testing.T) {
	generated, err := resolveSessionID("")
	if err != nil {
		t.Fatalf("resolveSessionID(\"\") error: %v", err)
	}
	if !safeSessionIDRegex.MatchString(generated) {
		t.Fatalf("generated session ID %q failed safe pattern", generated)
	}

	if got, err := resolveSessionID(" sess_123-abc "); err != nil || got != "sess_123-abc" {
		t.Fatalf("resolveSessionID valid = (%q, %v), want (\"sess_123-abc\", nil)", got, err)
	}

	for _, bad := range []string{"../../etc/passwd", "a/b", "has space", "..", "a.b", strings.Repeat("a", 129)} {
		if _, err := resolveSessionID(bad); err == nil {
			t.Errorf("resolveSessionID(%q) succeeded, expected error", bad)
		}
	}

	runner := &Runner{Config: Config{PiBin: filepath.Join(t.TempDir(), "nonexistent-pi")}}
	if _, err := runner.Run(context.Background(), Request{
		Workspace:    t.TempDir(),
		Instructions: "Check tests",
		SessionID:    "../../escape",
	}, nil); err == nil || !strings.Contains(err.Error(), "invalid sessionId") {
		t.Fatalf("expected invalid sessionId error before starting pi, got %v", err)
	}
}
