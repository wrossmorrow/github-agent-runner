package sink

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/wrossmorrow/github-agent-runner/addons/usage-sink/internal/als"
)

var safeDBNameRegex = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

const tokenUsageColumnsList = `timestamp, response_id, status, prompt_tokens, completion_tokens, total_tokens, cached_tokens, session_id, session_name, model, provider, request_received_at, first_response_at, last_response_at, error_at, first_response_latency_ms, total_response_latency_ms, reasoning_tokens, cache_creation_tokens, session_kind, github_repo, github_pr, github_run_id, client_id, response_code, upstream_host, token_latency_ttft_ms, token_latency_itl_ms`

const createTableSQLFmt = `
	CREATE TABLE IF NOT EXISTS %s (
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
`

var additiveColumns = []string{
	`"model" VARCHAR`,
	`"provider" VARCHAR`,
	`"reasoning_tokens" BIGINT`,
	`"cache_creation_tokens" BIGINT`,
	`"session_kind" VARCHAR`,
	`"github_repo" VARCHAR`,
	`"github_pr" VARCHAR`,
	`"github_run_id" VARCHAR`,
	`"client_id" VARCHAR`,
	`"response_code" INTEGER`,
	`"upstream_host" VARCHAR`,
	`"token_latency_ttft_ms" DOUBLE`,
	`"token_latency_itl_ms" DOUBLE`,
}

// DuckDBConfig configures local DuckDB buffering and optional MotherDuck flushing.
type DuckDBConfig struct {
	DBPath          string
	MotherDuckDB    string
	MotherDuckToken string
	FlushInterval   time.Duration
	Logger          *slog.Logger
	Registerer      prometheus.Registerer
}

// DuckDBSink buffers UsageRecords into local DuckDB and optionally flushes periodically to MotherDuck.
type DuckDBSink struct {
	db         *sql.DB
	mdDatabase string
	mdEnabled  bool
	logger     *slog.Logger

	flushesOK  prometheus.Counter
	flushesErr prometheus.Counter

	mu        sync.Mutex
	stopCh    chan struct{}
	loopDone  chan struct{}
	closeOnce sync.Once
}

// NewDuckDBSink opens/initializes the local DuckDB file, creates/migrates `token_usage`,
// and starts a periodic MotherDuck flush loop when both MotherDuckDB and MotherDuckToken are set.
func NewDuckDBSink(cfg DuckDBConfig) (*DuckDBSink, error) {
	if strings.TrimSpace(cfg.DBPath) == "" {
		cfg.DBPath = "/tmp/usage.duckdb"
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 60 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	mdDB := strings.TrimSpace(cfg.MotherDuckDB)
	mdTok := strings.TrimSpace(cfg.MotherDuckToken)
	mdEnabled := mdDB != "" && mdTok != ""
	if mdDB != "" && !safeDBNameRegex.MatchString(mdDB) {
		return nil, fmt.Errorf("invalid motherduck database name %q: must match [A-Za-z0-9_-]+", mdDB)
	}
	if mdDB != "" && mdTok == "" {
		cfg.Logger.Warn("MOTHERDUCK_DATABASE set without MOTHERDUCK_TOKEN; MotherDuck flush disabled (local DuckDB only)")
	}

	if cfg.DBPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o750); err != nil {
			return nil, fmt.Errorf("creating duckdb parent dir: %w", err)
		}
	}
	for _, dir := range []string{"/tmp/duckdb_ext", "/tmp/duckdb_tmp"} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	db, err := sql.Open("duckdb", cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("opening duckdb %s: %w", cfg.DBPath, err)
	}
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("acquiring duckdb conn: %w", err)
	}
	for _, stmt := range []string{
		`SET extension_directory='/tmp/duckdb_ext';`,
		`SET temp_directory='/tmp/duckdb_tmp';`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			_ = conn.Close()
			_ = db.Close()
			return nil, fmt.Errorf("configuring duckdb paths (%s): %w", stmt, err)
		}
	}
	if err := ensureTableSchema(ctx, conn, "token_usage"); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, err
	}
	_ = conn.Close()

	reg := cfg.Registerer
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	flushes := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "usage_sink_motherduck_flushes_total",
		Help: "Total MotherDuck flush attempts by status.",
	}, []string{"status"})
	reg.MustRegister(flushes)

	s := &DuckDBSink{
		db:         db,
		mdDatabase: mdDB,
		mdEnabled:  mdEnabled,
		logger:     cfg.Logger,
		flushesOK:  flushes.WithLabelValues("ok"),
		flushesErr: flushes.WithLabelValues("error"),
		stopCh:     make(chan struct{}),
		loopDone:   make(chan struct{}),
	}
	go s.flushLoop(cfg.FlushInterval)
	return s, nil
}

func ensureTableSchema(ctx context.Context, conn *sql.Conn, tableRef string) error {
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(createTableSQLFmt, tableRef)); err != nil {
		return fmt.Errorf("create table %s: %w", tableRef, err)
	}
	for _, colDef := range additiveColumns {
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s;", tableRef, colDef)
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate table %s (%s): %w", tableRef, colDef, err)
		}
	}
	return nil
}

func (s *DuckDBSink) Write(ctx context.Context, rec als.UsageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	const insertSQL = `
		INSERT OR IGNORE INTO token_usage (` + tokenUsageColumnsList + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`
	_, err := s.db.ExecContext(ctx, insertSQL,
		rec.Timestamp,
		rec.ResponseID,
		rec.Status,
		rec.PromptTokens,
		rec.CompletionTokens,
		rec.TotalTokens,
		rec.CachedTokens,
		rec.SessionID,
		rec.SessionName,
		rec.Model,
		rec.Provider,
		rec.RequestReceivedAt,
		nullableTime(rec.FirstResponseAt),
		nullableTime(rec.LastResponseAt),
		nullableTime(rec.ErrorAt),
		rec.FirstResponseLatencyMs,
		rec.TotalResponseLatencyMs,
		rec.ReasoningTokens,
		rec.CacheCreationTokens,
		rec.SessionKind,
		rec.GitHubRepo,
		rec.GitHubPR,
		rec.GitHubRunID,
		rec.ClientID,
		rec.ResponseCode,
		rec.UpstreamHost,
		rec.TokenLatencyTTFTMs,
		rec.TokenLatencyITLMs,
	)
	return err
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

func (s *DuckDBSink) flushLoop(interval time.Duration) {
	defer close(s.loopDone)
	if !s.mdEnabled {
		<-s.stopCh
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			fCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := s.Flush(fCtx); err != nil {
				s.logger.Warn("periodic motherduck flush failed", slog.Any("error", err))
			}
			cancel()
		}
	}
}

// Flush uploads pending local `token_usage` rows to MotherDuck idempotently and prunes
// uploaded rows from the local DuckDB buffer. It is a no-op when MotherDuck is disabled.
func (s *DuckDBSink) Flush(ctx context.Context) error {
	if !s.mdEnabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	conn, err := s.db.Conn(ctx)
	if err != nil {
		s.flushesErr.Inc()
		return fmt.Errorf("acquire duckdb conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "LOAD md;"); err != nil {
		if _, ierr := conn.ExecContext(ctx, "INSTALL md;"); ierr != nil {
			s.flushesErr.Inc()
			return fmt.Errorf("install md extension: %w", ierr)
		}
		if _, lerr := conn.ExecContext(ctx, "LOAD md;"); lerr != nil {
			s.flushesErr.Inc()
			return fmt.Errorf("load md extension: %w", lerr)
		}
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("ATTACH IF NOT EXISTS 'md:%s' AS \"%s\";", s.mdDatabase, s.mdDatabase)); err != nil {
		s.flushesErr.Inc()
		return fmt.Errorf("attach motherduck %q: %w", s.mdDatabase, err)
	}
	if err := flushToAttachedCatalog(ctx, conn, s.mdDatabase); err != nil {
		s.flushesErr.Inc()
		return err
	}
	s.flushesOK.Inc()
	return nil
}

// flushToAttachedCatalog ensures `<catalog>.token_usage` exists with additive columns,
// copies pending local rows idempotently by `response_id`, and prunes the flushed batch.
func flushToAttachedCatalog(ctx context.Context, conn *sql.Conn, catalog string) error {
	targetTable := fmt.Sprintf(`"%s".token_usage`, catalog)
	if err := ensureTableSchema(ctx, conn, targetTable); err != nil {
		return err
	}

	var pending int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM token_usage;").Scan(&pending); err != nil {
		return fmt.Errorf("count pending rows: %w", err)
	}
	if pending == 0 {
		return nil
	}

	if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS _usage_flush_batch;"); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "CREATE TEMP TABLE _usage_flush_batch AS SELECT * FROM token_usage;"); err != nil {
		return fmt.Errorf("snapshot flush batch: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "DROP TABLE IF EXISTS _usage_flush_batch;")
	}()

	upsertSQL := fmt.Sprintf(`
		INSERT INTO %s (%s)
		SELECT %s
		FROM _usage_flush_batch AS l
		WHERE NOT EXISTS (
			SELECT 1 FROM %s AS t WHERE t.response_id = l.response_id
		);`, targetTable, tokenUsageColumnsList, tokenUsageColumnsList, targetTable)
	if _, err := conn.ExecContext(ctx, upsertSQL); err != nil {
		return fmt.Errorf("upsert into %s: %w", targetTable, err)
	}

	if _, err := conn.ExecContext(ctx, "DELETE FROM token_usage WHERE response_id IN (SELECT response_id FROM _usage_flush_batch);"); err != nil {
		return fmt.Errorf("prune flushed local rows: %w", err)
	}
	return nil
}

// Close stops the periodic ticker, performs a best-effort final flush, and closes DuckDB.
func (s *DuckDBSink) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		close(s.stopCh)
	})
	<-s.loopDone
	flushErr := s.Flush(ctx)
	s.mu.Lock()
	closeErr := s.db.Close()
	s.mu.Unlock()
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}
