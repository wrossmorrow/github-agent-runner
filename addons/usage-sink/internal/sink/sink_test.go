package sink

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/wrossmorrow/github-agent-runner/addons/usage-sink/internal/als"
)

func TestJSONLSink(t *testing.T) {
	var buf bytes.Buffer
	s := NewJSONLSink(&buf)
	rec := als.UsageRecord{
		Timestamp:        time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC),
		ResponseID:       "resp-jsonl-1",
		Status:           "ok",
		PromptTokens:     50,
		CompletionTokens: 25,
		TotalTokens:      75,
		SessionID:        "sess-1",
		Model:            "gemini-4-neon",
	}
	if err := s.Write(context.Background(), rec); err != nil {
		t.Fatalf("JSONLSink.Write: %v", err)
	}

	var decoded als.UsageRecord
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal JSONL output: %v (raw=%q)", err, buf.String())
	}
	if decoded.ResponseID != "resp-jsonl-1" || decoded.TotalTokens != 75 || decoded.SessionID != "sess-1" {
		t.Fatalf("unexpected decoded record: %+v", decoded)
	}
}

func TestDuckDBSinkIdempotentAndFlushPrune(t *testing.T) {
	tmpDir := t.TempDir()
	localDBPath := filepath.Join(tmpDir, "local.duckdb")
	remoteDBPath := filepath.Join(tmpDir, "remote.duckdb")

	// Pre-create a legacy remote table missing additive columns to verify migration.
	remoteDB, err := sql.Open("duckdb", remoteDBPath)
	if err != nil {
		t.Fatalf("open remote duckdb: %v", err)
	}
	if _, err := remoteDB.Exec(`
		CREATE TABLE token_usage (
			timestamp TIMESTAMP,
			response_id VARCHAR PRIMARY KEY,
			status VARCHAR,
			prompt_tokens BIGINT,
			completion_tokens BIGINT,
			total_tokens BIGINT,
			cached_tokens BIGINT,
			session_id VARCHAR,
			session_name VARCHAR,
			model VARCHAR,
			provider VARCHAR,
			request_received_at TIMESTAMP,
			first_response_at TIMESTAMP,
			last_response_at TIMESTAMP,
			error_at TIMESTAMP,
			first_response_latency_ms BIGINT,
			total_response_latency_ms BIGINT
		);
		INSERT INTO token_usage (timestamp, response_id, status, prompt_tokens, completion_tokens, total_tokens, cached_tokens, session_id, session_name)
		VALUES ('2026-11-27 14:00:00', 'dup-id', 'ok', 10, 5, 15, 0, 'old-sess', 'old');
	`); err != nil {
		t.Fatalf("seed legacy remote table: %v", err)
	}
	_ = remoteDB.Close()

	reg := prometheus.NewRegistry()
	ds, err := NewDuckDBSink(DuckDBConfig{
		DBPath:     localDBPath,
		Registerer: reg,
	})
	if err != nil {
		t.Fatalf("NewDuckDBSink: %v", err)
	}
	defer func() { _ = ds.Close(context.Background()) }()

	now := time.Date(2026, 11, 27, 15, 30, 0, 0, time.UTC)
	firstAt := now.Add(100 * time.Millisecond)
	rDup := als.UsageRecord{
		Timestamp:         now,
		ResponseID:        "dup-id",
		Status:            "ok",
		PromptTokens:      999,
		TotalTokens:       999,
		RequestReceivedAt: now,
		FirstResponseAt:   &firstAt,
	}
	rNew := als.UsageRecord{
		Timestamp:          now,
		ResponseID:         "new-id",
		Status:             "ok",
		PromptTokens:       100,
		CompletionTokens:   50,
		TotalTokens:        150,
		CachedTokens:       20,
		ReasoningTokens:    12,
		SessionID:          "sess-42",
		SessionKind:        "review",
		SessionName:        "org/repo#42",
		GitHubRepo:         "org/repo",
		GitHubPR:           "42",
		Model:              "gemini-4-neon",
		Provider:           "vertex",
		ResponseCode:       200,
		UpstreamHost:       "10.0.0.5:8080",
		RequestReceivedAt:  now,
		TokenLatencyTTFTMs: 123.5,
	}

	// Duplicate local Write should be ignored cleanly without error.
	if err := ds.Write(context.Background(), rDup); err != nil {
		t.Fatalf("Write dup #1: %v", err)
	}
	if err := ds.Write(context.Background(), rDup); err != nil {
		t.Fatalf("Write dup #2: %v", err)
	}
	if err := ds.Write(context.Background(), rNew); err != nil {
		t.Fatalf("Write new: %v", err)
	}

	conn, err := ds.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(context.Background(), fmt.Sprintf(`ATTACH '%s' AS "target_db";`, remoteDBPath)); err != nil {
		t.Fatalf("attach remote test db: %v", err)
	}
	if err := flushToAttachedCatalog(context.Background(), conn, "target_db"); err != nil {
		t.Fatalf("flushToAttachedCatalog: %v", err)
	}

	// Local buffer must be pruned after flush.
	var localRemaining int
	if err := conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM token_usage;").Scan(&localRemaining); err != nil {
		t.Fatalf("count local remaining: %v", err)
	}
	if localRemaining != 0 {
		t.Fatalf("expected 0 remaining local rows after prune, got %d", localRemaining)
	}

	// Target table must have kept legacy 'dup-id' untouched and inserted 'new-id' with new columns.
	var remoteCount int
	if err := conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM "target_db".token_usage;`).Scan(&remoteCount); err != nil {
		t.Fatalf("count target rows: %v", err)
	}
	if remoteCount != 2 {
		t.Fatalf("expected 2 target rows (1 existing + 1 new), got %d", remoteCount)
	}
	var dupPrompt int64
	if err := conn.QueryRowContext(context.Background(), `SELECT prompt_tokens FROM "target_db".token_usage WHERE response_id = 'dup-id';`).Scan(&dupPrompt); err != nil {
		t.Fatalf("query dup-id: %v", err)
	}
	if dupPrompt != 10 {
		t.Fatalf("expected existing dup-id prompt_tokens=10 preserved, got %d", dupPrompt)
	}

	var gotKind, gotRepo string
	var gotReasoning int64
	if err := conn.QueryRowContext(context.Background(), `SELECT session_kind, github_repo, reasoning_tokens FROM "target_db".token_usage WHERE response_id = 'new-id';`).Scan(&gotKind, &gotRepo, &gotReasoning); err != nil {
		t.Fatalf("query new-id additive columns: %v", err)
	}
	if gotKind != "review" || gotRepo != "org/repo" || gotReasoning != 12 {
		t.Fatalf("unexpected migrated values on new-id: kind=%q repo=%q reasoning=%d", gotKind, gotRepo, gotReasoning)
	}
}
