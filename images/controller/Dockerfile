FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ ./api/
COPY pkg/ ./pkg/
COPY cmd/agentrunner-controller/ ./cmd/agentrunner-controller/
RUN CGO_ENABLED=0 go build -o /agentrunner-controller ./cmd/agentrunner-controller

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /agentrunner-controller /agentrunner-controller
USER 65532:65532
ENTRYPOINT ["/agentrunner-controller"]
