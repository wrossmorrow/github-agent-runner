FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS deps
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY --from=deps /go/pkg/mod /go/pkg/mod
COPY go.mod go.sum ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /usage-sink ./cmd/usage-sink

FROM debian:bookworm-slim
LABEL org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="agentrunner-usage-sink" \
      org.opencontainers.image.description="Reference Envoy AI Gateway ALS usage collector (JSONL / DuckDB / MotherDuck)"

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libstdc++6 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -u 65532 -r -s /usr/sbin/nologin nonroot

COPY --from=build /usage-sink /usr/local/bin/usage-sink

ENV HOME=/tmp
USER 65532:65532

EXPOSE 50051 9090
ENTRYPOINT ["/usr/local/bin/usage-sink"]
