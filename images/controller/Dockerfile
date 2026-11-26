FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ ./api/
COPY pkg/ ./pkg/
COPY cmd/agentrunner-controller/ ./cmd/agentrunner-controller/
# Verify cross-compiled target OS/arch and static linkage (CGO_ENABLED=0) without QEMU execution.
RUN os="${TARGETOS:-linux}" arch="${TARGETARCH:-$(go env GOARCH)}" \
    && CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" go build -o /agentrunner-controller ./cmd/agentrunner-controller \
    && meta="$(go version -m /agentrunner-controller)" \
    && printf '%s\n' "${meta}" | grep -q "CGO_ENABLED=0" \
    && printf '%s\n' "${meta}" | grep -q "GOOS=${os}" \
    && printf '%s\n' "${meta}" | grep -q "GOARCH=${arch}"

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.licenses="Apache-2.0"
COPY --from=builder /agentrunner-controller /agentrunner-controller
USER 65532:65532
ENTRYPOINT ["/agentrunner-controller"]
