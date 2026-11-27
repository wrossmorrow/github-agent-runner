package als

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"
	alsv3 "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v3"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func sampleAIGatewayEntry(reqID string, code uint32, includeMeta bool) *accesslogv3.HTTPAccessLogEntry {
	start := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	common := &accesslogv3.AccessLogCommon{
		StartTime:                  timestamppb.New(start),
		TimeToFirstUpstreamRxByte:  durationpb.New(250 * time.Millisecond),
		TimeToLastDownstreamTxByte: durationpb.New(1200 * time.Millisecond),
		UpstreamRemoteAddress: &corev3.Address{
			Address: &corev3.Address_SocketAddress{
				SocketAddress: &corev3.SocketAddress{
					Address:       "10.0.0.42",
					PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 8000},
				},
			},
		},
	}
	if includeMeta {
		common.Metadata = &corev3.Metadata{
			FilterMetadata: map[string]*structpb.Struct{
				aiGatewayMetadataKey: {
					Fields: map[string]*structpb.Value{
						"llm_input_token":                structpb.NewNumberValue(120),
						"llm_cached_input_token":         structpb.NewNumberValue(40),
						"llm_cache_creation_input_token": structpb.NewNumberValue(10),
						"llm_output_token":               structpb.NewNumberValue(35),
						"llm_reasoning_token":            structpb.NewNumberValue(15),
						"llm_total_token":                structpb.NewNumberValue(155),
						"response_model":                 structpb.NewStringValue("gemini-4-neon"),
						"ai_service_backend_name":        structpb.NewStringValue("vertex"),
						"token_latency_ttft":             structpb.NewNumberValue(210.5),
						"token_latency_itl":              structpb.NewNumberValue(12.25),
					},
				},
			},
		}
	}
	return &accesslogv3.HTTPAccessLogEntry{
		CommonProperties: common,
		Request: &accesslogv3.HTTPRequestProperties{
			RequestId: reqID,
			RequestHeaders: map[string]string{
				"X-Session-Id":    "sess-uuid-1",
				"x-session-kind":  "suggest",
				"x-session-name":  "org/repo#42",
				"x-github-repo":   "org/repo",
				"x-github-pr":     "42",
				"x-github-run-id": "99999",
				"x-client-id":     "arc-runner",
				"x-ai-eg-model":   "fallback-header-model",
			},
		},
		Response: &accesslogv3.HTTPResponseProperties{
			ResponseCode: wrapperspb.UInt32(code),
		},
	}
}

func TestParseHTTPEntry(t *testing.T) {
	rec, ok := ParseHTTPEntry(sampleAIGatewayEntry("resp-1", 200, true), true)
	if !ok {
		t.Fatal("expected AI gateway entry to be parsed")
	}
	if rec.ResponseID != "resp-1" || rec.Status != "ok" || rec.ResponseCode != 200 {
		t.Fatalf("unexpected core HTTP fields: %+v", rec)
	}
	if rec.PromptTokens != 120 || rec.CachedTokens != 40 || rec.CacheCreationTokens != 10 ||
		rec.CompletionTokens != 35 || rec.ReasoningTokens != 15 || rec.TotalTokens != 155 {
		t.Fatalf("unexpected token counts: %+v", rec)
	}
	if rec.Model != "gemini-4-neon" || rec.Provider != "vertex" || rec.UpstreamHost != "10.0.0.42:8000" {
		t.Fatalf("unexpected model/provider/host: %+v", rec)
	}
	if rec.SessionID != "sess-uuid-1" || rec.SessionKind != "suggest" || rec.SessionName != "org/repo#42" ||
		rec.GitHubRepo != "org/repo" || rec.GitHubPR != "42" || rec.GitHubRunID != "99999" || rec.ClientID != "arc-runner" {
		t.Fatalf("unexpected attribution fields: %+v", rec)
	}
	if rec.FirstResponseLatencyMs != 250 || rec.TotalResponseLatencyMs != 1200 ||
		rec.TokenLatencyTTFTMs != 210.5 || rec.TokenLatencyITLMs != 12.25 || rec.ErrorAt != nil {
		t.Fatalf("unexpected latency/error fields: %+v", rec)
	}

	// Model fallback to x-ai-eg-model and non-2xx sets error_at.
	fallbackEntry := sampleAIGatewayEntry("", 502, true)
	delete(fallbackEntry.CommonProperties.Metadata.FilterMetadata[aiGatewayMetadataKey].Fields, "response_model")
	recErr, ok := ParseHTTPEntry(fallbackEntry, true)
	if !ok {
		t.Fatal("expected fallback entry to be parsed")
	}
	if recErr.ResponseID == "" || recErr.Model != "fallback-header-model" || recErr.Status != "error" || recErr.ErrorAt == nil {
		t.Fatalf("unexpected error/fallback record: %+v", recErr)
	}

	// Non-AI entry skipped when onlyAIGateway=true, accepted when false.
	plain := sampleAIGatewayEntry("health-1", 200, false)
	if _, ok := ParseHTTPEntry(plain, true); ok {
		t.Fatal("expected non-AI entry to be skipped when onlyAIGateway=true")
	}
	if recPlain, ok := ParseHTTPEntry(plain, false); !ok || recPlain.ResponseID != "health-1" {
		t.Fatalf("expected non-AI entry when onlyAIGateway=false, got ok=%v rec=%+v", ok, recPlain)
	}
}

type captureWriter struct {
	mu      sync.Mutex
	records []UsageRecord
}

func (c *captureWriter) Write(_ context.Context, rec UsageRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)
	return nil
}

func TestStreamAccessLogsEndToEnd(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	cw := &captureWriter{}
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	alsSrv := NewServer(cw, true, 64, nil, metrics)

	grpcSrv := grpc.NewServer()
	alsv3.RegisterAccessLogServiceServer(grpcSrv, alsSrv)
	go func() {
		_ = grpcSrv.Serve(lis)
	}()

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer func() { _ = conn.Close() }()

	client := alsv3.NewAccessLogServiceClient(conn)
	stream, err := client.StreamAccessLogs(context.Background())
	if err != nil {
		t.Fatalf("StreamAccessLogs: %v", err)
	}

	for _, batch := range [][]*accesslogv3.HTTPAccessLogEntry{
		{sampleAIGatewayEntry("r1", 200, true), sampleAIGatewayEntry("ignored", 200, false)},
		{sampleAIGatewayEntry("r2", 200, true), sampleAIGatewayEntry("r3", 429, true)},
	} {
		err := stream.Send(&alsv3.StreamAccessLogsMessage{
			LogEntries: &alsv3.StreamAccessLogsMessage_HttpLogs{
				HttpLogs: &alsv3.StreamAccessLogsMessage_HTTPAccessLogEntries{
					LogEntry: batch,
				},
			},
		})
		if err != nil {
			t.Fatalf("stream.Send: %v", err)
		}
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}

	grpcSrv.GracefulStop()
	if err := alsSrv.Close(context.Background()); err != nil {
		t.Fatalf("alsSrv.Close: %v", err)
	}

	cw.mu.Lock()
	defer cw.mu.Unlock()
	if len(cw.records) != 3 {
		t.Fatalf("expected 3 AI gateway records, got %d: %+v", len(cw.records), cw.records)
	}
	if cw.records[0].ResponseID != "r1" || cw.records[1].ResponseID != "r2" || cw.records[2].ResponseID != "r3" {
		t.Fatalf("unexpected response IDs: %+v", cw.records)
	}
}
