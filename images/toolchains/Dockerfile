ARG TARGETARCH

ARG NODE_TAG=22-bookworm
ARG PI_VERSION=1.6.1
ARG JUST_VERSION=1.59.0
ARG GH_VERSION=2.105.0

ARG SHELLCHECK_VERSION=v0.10.0
ARG HADOLINT_VERSION=v2.12.0
ARG ACTIONLINT_VERSION=1.7.12
ARG YQ_VERSION=4.54.3

ARG UV_VERSION=0.13.10
ARG PYTHON_VERSION=3.13.18
ARG GO_VERSION=1.27.4
ARG GOLANGCI_LINT_VERSION=2.15.0
ARG RUST_VERSION=1.99.1
ARG JAVA_VERSION=21

ARG PROTOC_VERSION=36.2
ARG BUF_VERSION=1.75.0
ARG PROTOC_GEN_GO_VERSION=1.36.12
ARG PROTOC_GEN_GO_GRPC_VERSION=1.6.2
ARG PROTOC_GEN_CONNECT_GO_VERSION=1.21.1

ARG GRADLE_VERSION=8.0.2

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Pinned multi-arch helper stages (BuildKit fetches only stages used by the requested target)
FROM bufbuild/buf:${BUF_VERSION} AS buf
FROM koalaman/shellcheck:${SHELLCHECK_VERSION} AS shellcheck
FROM hadolint/hadolint:${HADOLINT_VERSION} AS hadolint
FROM rhysd/actionlint:${ACTIONLINT_VERSION} AS actionlint
FROM mikefarah/yq:${YQ_VERSION} AS yq
FROM ghcr.io/astral-sh/uv:${UV_VERSION} AS uv
FROM golang:${GO_VERSION}-bookworm AS go-dist

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Build agentrunnerd (Go HTTP/SSE daemon that spawns `pi --mode rpc`).
# Baked into the base agent image below so every derived toolchain stage ships with it.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS agentrunnerd-build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY api/ ./api/
COPY pkg/ ./pkg/
COPY cmd/agentrunnerd/ ./cmd/agentrunnerd/
# Verify cross-compiled target OS/arch and static linkage (CGO_ENABLED=0) without QEMU execution.
RUN os="${TARGETOS:-linux}" arch="${TARGETARCH:-$(go env GOARCH)}" \
    && CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" go build -o /agentrunnerd ./cmd/agentrunnerd \
    && meta="$(go version -m /agentrunnerd)" \
    && printf '%s\n' "${meta}" | grep -q "CGO_ENABLED=0" \
    && printf '%s\n' "${meta}" | grep -q "GOOS=${os}" \
    && printf '%s\n' "${meta}" | grep -q "GOARCH=${arch}"

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Main agent base container stage
FROM node:${NODE_TAG} AS agent
LABEL org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="agentrunner-agent" \
      org.opencontainers.image.description="Base agentrunner sidecar with agentrunnerd and pi"

ENV PAGER=cat GIT_PAGER=cat LESS=-FRX GIT_TERMINAL_PROMPT=0 DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG TARGETARCH
ARG PROTOC_VERSION
ARG JUST_VERSION
ARG GH_VERSION
ARG PI_VERSION

# Base CLI tools, protoc, and multi-arch just
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ripgrep curl wget fd-find unzip jq ca-certificates \
    && ln -s /usr/bin/fdfind /usr/local/bin/fd \
    && rm -rf /var/lib/apt/lists/* \
    && case "${TARGETARCH}" in \
        arm64) PARCH=aarch_64; JARCH=aarch64 ;; \
        amd64) PARCH=x86_64;  JARCH=x86_64 ;; \
        *) echo "unsupported arch ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL -o /tmp/protoc.zip \
        "https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-${PARCH}.zip" \
    && unzip -q /tmp/protoc.zip -d /usr/local bin/protoc 'include/*' \
    && chmod 755 /usr/local/bin/protoc \
    && rm /tmp/protoc.zip \
    && curl -fsSL "https://github.com/casey/just/releases/download/${JUST_VERSION}/just-${JUST_VERSION}-${JARCH}-unknown-linux-musl.tar.gz" -o /tmp/just.tar.gz \
    && tar -xzf /tmp/just.tar.gz -C /tmp \
    && mv /tmp/just /usr/local/bin/just \
    && rm -rf /tmp/just.tar.gz

COPY --from=buf /usr/local/bin/buf /usr/local/bin/buf
COPY --from=shellcheck /bin/shellcheck /usr/local/bin/shellcheck
COPY --from=hadolint /bin/hadolint /usr/local/bin/hadolint
COPY --from=actionlint /usr/local/bin/actionlint /usr/local/bin/actionlint
COPY --from=yq /usr/bin/yq /usr/local/bin/yq

RUN mkdir -p /usr/local/lib/gh \
    && curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${TARGETARCH}.tar.gz" \
        | tar -xz -C /tmp \
    && mv "/tmp/gh_${GH_VERSION}_linux_${TARGETARCH}/bin/gh" /usr/local/lib/gh/gh \
    && rm -rf /tmp/gh_*

COPY --chmod=755 images/toolchains/gh-shim.sh /usr/local/bin/gh

RUN npm install -g "@earendil-works/pi-coding-agent@${PI_VERSION}" \
    && npm cache clean --force

# Allow git inside the node (UID 1000) sidecar to inspect workspaces checked out by ARC's runner (UID 1001).
RUN git config --system --add safe.directory '*' \
    && mkdir -p /home/node/.pi /opt/agents /home/runner/_work \
    && chown node:node /home/node/.pi /home/runner/_work

COPY --from=agentrunnerd-build /agentrunnerd /usr/local/bin/agentrunnerd

USER node
WORKDIR /home/runner/_work

ENTRYPOINT ["/usr/local/bin/agentrunnerd"]

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Python toolchain: derives from the agent image and installs the pinned Python minor via uv.
FROM agent AS python
USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG PYTHON_VERSION
LABEL io.agentrunner.toolchain="python-${PYTHON_VERSION}"

COPY --from=uv /uv /uvx /usr/local/bin/

ENV UV_PYTHON_INSTALL_DIR=/usr/local/python
RUN uv python install --managed-python "${PYTHON_VERSION}" \
    && pybin="$(uv python find --managed-python "${PYTHON_VERSION}")" \
    && ln -sf "${pybin}" /usr/local/bin/python3 \
    && ln -sf /usr/local/bin/python3 /usr/local/bin/python \
    && chmod -R a+rX /usr/local/python
ENV UV_PYTHON_DOWNLOADS=never

USER node
WORKDIR /home/runner/_work

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Go toolchain: derives from the agent image and adds the pinned Go distribution + protoc plugins.
FROM agent AS golang
USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG GO_VERSION
ARG GOLANGCI_LINT_VERSION
ARG PROTOC_GEN_GO_VERSION
ARG PROTOC_GEN_GO_GRPC_VERSION
ARG PROTOC_GEN_CONNECT_GO_VERSION
LABEL io.agentrunner.toolchain="golang-${GO_VERSION}"

COPY --from=go-dist /usr/local/go /usr/local/go
RUN ln -s /usr/local/go/bin/go /usr/local/bin/go \
    && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt

ENV GOTOOLCHAIN=local

RUN GOBIN=/usr/local/bin go install "google.golang.org/protobuf/cmd/protoc-gen-go@v${PROTOC_GEN_GO_VERSION}" \
    && GOBIN=/usr/local/bin go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@v${PROTOC_GEN_GO_GRPC_VERSION}" \
    && GOBIN=/usr/local/bin go install "connectrpc.com/connect/cmd/protoc-gen-connect-go@v${PROTOC_GEN_CONNECT_GO_VERSION}" \
    && GOBIN=/usr/local/bin go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v${GOLANGCI_LINT_VERSION}" \
    && rm -rf /root/go /root/.cache/go-build

USER node
WORKDIR /home/runner/_work

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Rust toolchain: derives from the agent image and adds the pinned Rust toolchain on top.
FROM agent AS rust
USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG RUST_VERSION
LABEL io.agentrunner.toolchain="rust-${RUST_VERSION}"

ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    PATH="/usr/local/cargo/bin:/usr/local/rustup/bin:${PATH}"
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain "${RUST_VERSION}" --profile minimal \
    && rustup component add rustfmt clippy \
    && chown -R node:node /usr/local/cargo /usr/local/rustup \
    && printf 'export RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo\nexport PATH="/usr/local/cargo/bin:/usr/local/rustup/bin:${PATH}"\n' > /etc/profile.d/rust.sh \
    && chmod 644 /etc/profile.d/rust.sh

ENV PROTOC=/usr/local/bin/protoc

USER node
WORKDIR /home/runner/_work

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Java toolchain: derives from the agent image and adds a parameterized Adoptium JDK + Gradle on top.
FROM agent AS java
USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG TARGETARCH
ARG JAVA_VERSION
ARG GRADLE_VERSION
LABEL io.agentrunner.toolchain="java-${JAVA_VERSION}"

RUN case "${TARGETARCH}" in \
      amd64) export JDK_ARCH=x64 ;; \
      arm64) export JDK_ARCH=aarch64 ;; \
      *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL "https://api.adoptium.net/v3/binary/latest/${JAVA_VERSION}/ga/linux/${JDK_ARCH}/jdk/hotspot/normal/eclipse" \
        -o /tmp/jdk.tar.gz \
    && mkdir -p /opt/jdk \
    && tar -xzf /tmp/jdk.tar.gz -C /opt/jdk --strip-components=1 \
    && rm -rf /tmp/jdk.tar.gz

ENV JAVA_HOME=/opt/jdk \
    PATH="/opt/jdk/bin:${PATH}"

# Gradle binary distribution so the gradle wrapper can resolve locally when offline
RUN curl -fsSL "https://services.gradle.org/distributions/gradle-${GRADLE_VERSION}-bin.zip" -o /tmp/gradle.zip \
    && unzip -q /tmp/gradle.zip -d /opt \
    && ln -s "/opt/gradle-${GRADLE_VERSION}/bin/gradle" /usr/local/bin/gradle \
    && rm -f /tmp/gradle.zip

ENV GRADLE_HOME=/opt/gradle-${GRADLE_VERSION} \
    PATH="/opt/gradle-${GRADLE_VERSION}/bin:${PATH}"

RUN printf 'export JAVA_HOME=/opt/jdk GRADLE_HOME=/opt/gradle-%s\nexport PATH="/opt/jdk/bin:/opt/gradle-%s/bin:${PATH}"\n' "${GRADLE_VERSION}" "${GRADLE_VERSION}" > /etc/profile.d/java.sh \
    && chmod 644 /etc/profile.d/java.sh

USER node
WORKDIR /home/runner/_work

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Final stage: the full agent image (default build target for bare docker build).
FROM agent AS final
USER node
WORKDIR /home/runner/_work
