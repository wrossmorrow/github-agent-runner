package als

import (
	"strconv"
	"strings"
	"time"

	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/structpb"
)

const aiGatewayMetadataKey = "io.envoy.ai_gateway"

// UsageRecord represents one parsed Envoy AI Gateway HTTP access log event.
type UsageRecord struct {
	Timestamp              time.Time  `json:"timestamp"`
	ResponseID             string     `json:"response_id"`
	Status                 string     `json:"status"`
	PromptTokens           int64      `json:"prompt_tokens"`
	CompletionTokens       int64      `json:"completion_tokens"`
	TotalTokens            int64      `json:"total_tokens"`
	CachedTokens           int64      `json:"cached_tokens"`
	CacheCreationTokens    int64      `json:"cache_creation_tokens"`
	ReasoningTokens        int64      `json:"reasoning_tokens"`
	SessionID              string     `json:"session_id,omitempty"`
	SessionKind            string     `json:"session_kind,omitempty"`
	SessionName            string     `json:"session_name,omitempty"`
	Model                  string     `json:"model,omitempty"`
	Provider               string     `json:"provider,omitempty"`
	GitHubRepo             string     `json:"github_repo,omitempty"`
	GitHubPR               string     `json:"github_pr,omitempty"`
	GitHubRunID            string     `json:"github_run_id,omitempty"`
	ClientID               string     `json:"client_id,omitempty"`
	ResponseCode           int32      `json:"response_code"`
	UpstreamHost           string     `json:"upstream_host,omitempty"`
	RequestReceivedAt      time.Time  `json:"request_received_at"`
	FirstResponseAt        *time.Time `json:"first_response_at,omitempty"`
	LastResponseAt         *time.Time `json:"last_response_at,omitempty"`
	ErrorAt                *time.Time `json:"error_at,omitempty"`
	FirstResponseLatencyMs int64      `json:"first_response_latency_ms"`
	TotalResponseLatencyMs int64      `json:"total_response_latency_ms"`
	TokenLatencyTTFTMs     float64    `json:"token_latency_ttft_ms,omitempty"`
	TokenLatencyITLMs      float64    `json:"token_latency_itl_ms,omitempty"`
}

// ParseHTTPEntry extracts a UsageRecord from an Envoy HTTPAccessLogEntry.
// When onlyAIGateway is true, entries without `io.envoy.ai_gateway` dynamic metadata
// and zero token counts return ok=false.
func ParseHTTPEntry(entry *accesslogv3.HTTPAccessLogEntry, onlyAIGateway bool) (UsageRecord, bool) {
	if entry == nil {
		return UsageRecord{}, false
	}

	var aiMeta *structpb.Struct
	common := entry.GetCommonProperties()
	if common != nil && common.GetMetadata() != nil {
		aiMeta = common.GetMetadata().GetFilterMetadata()[aiGatewayMetadataKey]
	}

	headers := map[string]string{}
	req := entry.GetRequest()
	if req != nil {
		for k, v := range req.GetRequestHeaders() {
			headers[strings.ToLower(k)] = v
		}
	}

	promptTokens := structInt64(aiMeta, "llm_input_token")
	cachedTokens := structInt64(aiMeta, "llm_cached_input_token")
	cacheCreationTokens := structInt64(aiMeta, "llm_cache_creation_input_token")
	completionTokens := structInt64(aiMeta, "llm_output_token")
	reasoningTokens := structInt64(aiMeta, "llm_reasoning_token")
	totalTokens := structInt64(aiMeta, "llm_total_token")

	hasTokens := promptTokens > 0 || completionTokens > 0 || totalTokens > 0 || cachedTokens > 0 || cacheCreationTokens > 0 || reasoningTokens > 0
	if onlyAIGateway && aiMeta == nil && !hasTokens {
		return UsageRecord{}, false
	}

	model := structString(aiMeta, "response_model")
	if model == "" {
		model = structString(aiMeta, "model_name_override")
	}
	if model == "" {
		model = headers["x-ai-eg-model"]
	}

	provider := structString(aiMeta, "ai_service_backend_name")
	ttftMs := structFloat64(aiMeta, "token_latency_ttft")
	itlMs := structFloat64(aiMeta, "token_latency_itl")

	startTime := time.Now().UTC()
	if common != nil && common.GetStartTime() != nil {
		startTime = common.GetStartTime().AsTime().UTC()
	}

	var firstRespAt *time.Time
	var firstRespMs int64
	if common != nil && common.GetTimeToFirstUpstreamRxByte() != nil {
		d := common.GetTimeToFirstUpstreamRxByte().AsDuration()
		t := startTime.Add(d)
		firstRespAt = &t
		firstRespMs = d.Milliseconds()
	}

	var lastRespAt *time.Time
	var totalRespMs int64
	if common != nil && common.GetTimeToLastDownstreamTxByte() != nil {
		d := common.GetTimeToLastDownstreamTxByte().AsDuration()
		t := startTime.Add(d)
		lastRespAt = &t
		totalRespMs = d.Milliseconds()
	}

	var respCode int32
	if entry.GetResponse() != nil && entry.GetResponse().GetResponseCode() != nil {
		respCode = int32(entry.GetResponse().GetResponseCode().GetValue())
	}

	status := "ok"
	var errorAt *time.Time
	if respCode < 200 || respCode >= 300 {
		status = "error"
		t := startTime
		if lastRespAt != nil {
			t = *lastRespAt
		}
		errorAt = &t
	}

	responseID := ""
	if req != nil {
		responseID = strings.TrimSpace(req.GetRequestId())
	}
	if responseID == "" {
		responseID = uuid.NewString()
	}

	upstreamHost := ""
	if common != nil && common.GetUpstreamRemoteAddress() != nil {
		if sa := common.GetUpstreamRemoteAddress().GetSocketAddress(); sa != nil {
			if sa.GetPortValue() > 0 {
				upstreamHost = sa.GetAddress() + ":" + strconv.FormatUint(uint64(sa.GetPortValue()), 10)
			} else {
				upstreamHost = sa.GetAddress()
			}
		}
	}

	return UsageRecord{
		Timestamp:              startTime,
		ResponseID:             responseID,
		Status:                 status,
		PromptTokens:           promptTokens,
		CompletionTokens:       completionTokens,
		TotalTokens:            totalTokens,
		CachedTokens:           cachedTokens,
		CacheCreationTokens:    cacheCreationTokens,
		ReasoningTokens:        reasoningTokens,
		SessionID:              headers["x-session-id"],
		SessionKind:            headers["x-session-kind"],
		SessionName:            headers["x-session-name"],
		Model:                  model,
		Provider:               provider,
		GitHubRepo:             headers["x-github-repo"],
		GitHubPR:               headers["x-github-pr"],
		GitHubRunID:            headers["x-github-run-id"],
		ClientID:               headers["x-client-id"],
		ResponseCode:           respCode,
		UpstreamHost:           upstreamHost,
		RequestReceivedAt:      startTime,
		FirstResponseAt:        firstRespAt,
		LastResponseAt:         lastRespAt,
		ErrorAt:                errorAt,
		FirstResponseLatencyMs: firstRespMs,
		TotalResponseLatencyMs: totalRespMs,
		TokenLatencyTTFTMs:     ttftMs,
		TokenLatencyITLMs:      itlMs,
	}, true
}

func structString(s *structpb.Struct, key string) string {
	if s == nil {
		return ""
	}
	v, ok := s.GetFields()[key]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(v.GetStringValue())
}

func structFloat64(s *structpb.Struct, key string) float64 {
	if s == nil {
		return 0
	}
	v, ok := s.GetFields()[key]
	if !ok || v == nil {
		return 0
	}
	switch kind := v.GetKind().(type) {
	case *structpb.Value_NumberValue:
		return kind.NumberValue
	case *structpb.Value_StringValue:
		if f, err := strconv.ParseFloat(strings.TrimSpace(kind.StringValue), 64); err == nil {
			return f
		}
	}
	return 0
}

func structInt64(s *structpb.Struct, key string) int64 {
	return int64(structFloat64(s, key))
}
