# `usage-sink` Reference Add-on

Optional out-of-band telemetry collector for **Envoy Gateway + Envoy AI Gateway (Agent Router)**. Receives Envoy gRPC `envoy.service.accesslog.v3.AccessLogService` (`StreamAccessLogs`) batches on `:50051` (h2c), parses token usage, latencies, and `agentrunner` session/GitHub attribution headers, and emits structured records to stdout JSONL and/or local DuckDB with optional periodic flush to **MotherDuck**.

It lives in its own Go module (`addons/usage-sink/go.mod`) so CGO (`go-duckdb`) and Envoy protobuf dependencies never touch the core pure-Go `agentrunner` module.

---

## Configuration

| Variable | Default | Description |
| :--- | :--- | :--- |
| `USAGE_SINK_GRPC_ADDR` | `:50051` | gRPC h2c listen address for Envoy ALS |
| `USAGE_SINK_HTTP_ADDR` | `:9090` | HTTP listen address serving `/healthz` and Prometheus `/metrics` |
| `USAGE_SINK_MODE` | `stdout` *(or `duckdb` if `MOTHERDUCK_DATABASE` / `USAGE_SINK_DUCKDB_PATH` is set)* | Output sink: `stdout` \| `duckdb` \| `both` |
| `USAGE_SINK_DUCKDB_PATH` | `/tmp/usage.duckdb` | Local DuckDB buffer path (compatible with `readOnlyRootFilesystem: true` + `emptyDir` on `/tmp`) |
| `MOTHERDUCK_DATABASE` | `""` | Target MotherDuck database name (`[A-Za-z0-9_-]+`); enables `md:<db>` flush when paired with `MOTHERDUCK_TOKEN` |
| `MOTHERDUCK_TOKEN` | `""` | MotherDuck service token (read natively by DuckDB `md` extension) |
| `USAGE_SINK_FLUSH_INTERVAL` | `60s` | Periodic MotherDuck flush interval (final flush also runs on `SIGTERM`/`SIGINT`) |
| `USAGE_SINK_ONLY_AI_GATEWAY` | `true` | Drop HTTP access logs lacking `io.envoy.ai_gateway` metadata |

---

## Envoy Gateway / AI Gateway Integration

### 1. Kubernetes `Service` (`appProtocol: kubernetes.io/h2c`)
```yaml
apiVersion: v1
kind: Service
metadata:
  name: usage-sink
  namespace: ai-gateway
spec:
  selector:
    app: usage-sink
  ports:
    - name: grpc-als
      port: 50051
      targetPort: 50051
      appProtocol: kubernetes.io/h2c
    - name: metrics
      port: 9090
      targetPort: 9090
```

### 2. `EnvoyProxy` Access Log Sink (`als`)
Ensure `http.requestHeaders` includes all `agentrunner` attribution headers so Envoy forwards them on `HTTPAccessLogEntry`:

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyProxy
metadata:
  name: agent-router-proxy
  namespace: ai-gateway
spec:
  telemetry:
    accessLog:
      settings:
        - sinks:
            - type: ALS
              als:
                backendRefs:
                  - name: usage-sink
                    port: 50051
                type: HTTP
                http:
                  requestHeaders:
                    - x-session-id
                    - x-session-kind
                    - x-session-name
                    - x-github-repo
                    - x-github-pr
                    - x-github-run-id
                    - x-client-id
                    - x-ai-eg-model
```

### 3. `AIGatewayRoute` Dynamic Metadata (`llmRequestCosts`)
```yaml
spec:
  llmRequestCosts:
    - metadataKey: llm_input_token
      type: InputToken
    - metadataKey: llm_cached_input_token
      type: CachedInputToken
    - metadataKey: llm_cache_creation_input_token
      type: CacheCreationInputToken
    - metadataKey: llm_output_token
      type: OutputToken
    - metadataKey: llm_reasoning_token
      type: ReasoningToken
    - metadataKey: llm_total_token
      type: TotalToken
```

---

## Local Development & Image Build

```bash
just check-usage-sink              # gofmt, go vet, go build, go test -race -shuffle=on
just containerize-usage-sink dev   # build ghcr.io/wrossmorrow/agentrunner-usage-sink:dev

docker run --rm --read-only --tmpfs /tmp -p 9090:9090 \
  ghcr.io/wrossmorrow/agentrunner-usage-sink:dev
curl -fsS http://127.0.0.1:9090/healthz
```
