// Command usage-sink receives Envoy AccessLogService (ALS) telemetry over gRPC (h2c),
// parses AI Gateway token usage and agentrunner attribution metadata, and writes to JSONL and/or DuckDB/MotherDuck.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	alsv3 "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/wrossmorrow/github-agent-runner/addons/usage-sink/internal/als"
	"github.com/wrossmorrow/github-agent-runner/addons/usage-sink/internal/sink"
)

func main() {
	var (
		grpcAddr      = flag.String("grpc-addr", envOr("USAGE_SINK_GRPC_ADDR", ":50051"), "gRPC h2c listen address for Envoy ALS")
		httpAddr      = flag.String("http-addr", envOr("USAGE_SINK_HTTP_ADDR", ":9090"), "HTTP listen address for /healthz and /metrics")
		mode          = flag.String("mode", os.Getenv("USAGE_SINK_MODE"), "Sink mode: stdout | duckdb | both (defaults to duckdb when DB/MotherDuck env set, else stdout)")
		dbPath        = flag.String("duckdb-path", envOr("USAGE_SINK_DUCKDB_PATH", "/tmp/usage.duckdb"), "Local DuckDB buffer file path")
		mdDatabase    = flag.String("motherduck-db", os.Getenv("MOTHERDUCK_DATABASE"), "Target MotherDuck database name")
		flushInterval = flag.Duration("flush-interval", envDuration("USAGE_SINK_FLUSH_INTERVAL", 60*time.Second), "Periodic MotherDuck flush interval")
		onlyAIGW      = flag.Bool("only-ai-gateway", envBool("USAGE_SINK_ONLY_AI_GATEWAY", true), "Skip HTTP access logs without io.envoy.ai_gateway dynamic metadata")
	)
	flag.Parse()

	explicitFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	duckdbPathSet := strings.TrimSpace(os.Getenv("USAGE_SINK_DUCKDB_PATH")) != "" || explicitFlags["duckdb-path"]
	effectiveMode := resolveMode(*mode, duckdbPathSet, *mdDatabase)

	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	alsMetrics := als.NewMetrics(reg)

	var sinks []sink.Sink
	switch effectiveMode {
	case "stdout":
		sinks = append(sinks, sink.NewJSONLSink(os.Stdout))
	case "duckdb", "both":
		if effectiveMode == "both" {
			sinks = append(sinks, sink.NewJSONLSink(os.Stdout))
		}
		ds, err := sink.NewDuckDBSink(sink.DuckDBConfig{
			DBPath:          *dbPath,
			MotherDuckDB:    *mdDatabase,
			MotherDuckToken: os.Getenv("MOTHERDUCK_TOKEN"),
			FlushInterval:   *flushInterval,
			Logger:          logger,
			Registerer:      reg,
		})
		if err != nil {
			logger.Error("failed to initialize duckdb sink", slog.Any("error", err))
			os.Exit(1)
		}
		sinks = append(sinks, ds)
	default:
		logger.Error("invalid USAGE_SINK_MODE", slog.String("mode", effectiveMode))
		os.Exit(1)
	}
	multi := sink.NewMultiSink(sinks...)

	alsSrv := als.NewServer(multi, *onlyAIGW, 0, logger, alsMetrics)

	grpcLis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		logger.Error("failed to listen on gRPC address", slog.String("addr", *grpcAddr), slog.Any("error", err))
		os.Exit(1)
	}
	grpcSrv := grpc.NewServer()
	alsv3.RegisterAccessLogServiceServer(grpcSrv, alsSrv)
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcSrv, hs)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	httpSrv := &http.Server{
		Addr:              *httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("starting gRPC Envoy ALS listener", slog.String("addr", *grpcAddr), slog.String("mode", effectiveMode))
		if err := grpcSrv.Serve(grpcLis); err != nil {
			errCh <- fmt.Errorf("grpc serve: %w", err)
		}
	}()
	go func() {
		logger.Info("starting HTTP metrics/health listener", slog.String("addr", *httpAddr))
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("http serve: %w", err)
		}
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	select {
	case <-sigCtx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		logger.Error("listener exited unexpectedly", slog.Any("error", err))
		stop()
	}

	hs.Shutdown()
	grpcStopCtx, grpcStopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	stopped := make(chan struct{})
	go func() {
		grpcSrv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-grpcStopCtx.Done():
		grpcSrv.Stop()
	}
	grpcStopCancel()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer drainCancel()

	if err := alsSrv.Close(drainCtx); err != nil {
		logger.Warn("als queue drain incomplete", slog.Any("error", err))
	}
	if err := multi.Close(drainCtx); err != nil {
		logger.Warn("sink close/flush reported errors", slog.Any("error", err))
	}
	if err := httpSrv.Shutdown(drainCtx); err != nil {
		logger.Warn("http shutdown error", slog.Any("error", err))
	}
}

func resolveMode(explicit string, duckdbPathSet bool, mdDatabase string) string {
	m := strings.ToLower(strings.TrimSpace(explicit))
	if m != "" {
		return m
	}
	if strings.TrimSpace(mdDatabase) != "" || duckdbPathSet {
		return "duckdb"
	}
	return "stdout"
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
