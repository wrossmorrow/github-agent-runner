ARG TARGETARCH

ARG NODE_TAG=22-bookworm
ARG PI_VERSION=latest
ARG JUST_VERSION=1.59.0
ARG GH_VERSION=2.105.0

ARG GO_VERSION=1.26.10
ARG RUST_VERSION=1.99.1

ARG PROTOC_VERSION=36.2
ARG BUF_VERSION=1.75.0
ARG PROTOC_GEN_GO_VERSION=1.36.12
ARG PROTOC_GEN_GO_GRPC_VERSION=1.6.2
ARG PROTOC_GEN_CONNECT_GO_VERSION=1.21.1

ARG GRADLE_VERSION=8.0.2

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Shell stage to ref buf version with an ARG
FROM bufbuild/buf:${BUF_VERSION} AS buf

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
LABEL org.opencontainers.image.licenses="Apache-2.0"

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
COPY --from=koalaman/shellcheck:stable /bin/shellcheck /usr/local/bin/
COPY --from=hadolint/hadolint:latest   /bin/hadolint   /usr/local/bin/
COPY --from=rhysd/actionlint:latest    /usr/local/bin/actionlint /usr/local/bin/
COPY --from=mikefarah/yq:4             /usr/bin/yq     /usr/local/bin/

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

# Python toolchain: derives from the agent image and adds Python + uv on top.
FROM agent AS python
USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-pip python-is-python3 \
    && rm -rf /var/lib/apt/lists/*

ENV UV_INSTALL_DIR=/usr/local/uv
RUN curl -LsSf https://astral.sh/uv/install.sh | sh \
    && ln -s /usr/local/uv/uv /usr/local/bin/uv \
    && ln -s /usr/local/uv/uvx /usr/local/bin/uvx

USER node
WORKDIR /home/runner/_work

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Go toolchain: derives from the agent image and adds the Go toolchain on top.
FROM agent AS golang
USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG TARGETARCH
ARG GO_VERSION
ARG PROTOC_GEN_GO_VERSION
ARG PROTOC_GEN_GO_GRPC_VERSION
ARG PROTOC_GEN_CONNECT_GO_VERSION

RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
    | tar -xz -C /usr/local \
    && ln -s /usr/local/go/bin/go /usr/local/bin/go \
    && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt

RUN GOBIN=/usr/local/bin go install "google.golang.org/protobuf/cmd/protoc-gen-go@v${PROTOC_GEN_GO_VERSION}" \
    && GOBIN=/usr/local/bin go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@v${PROTOC_GEN_GO_GRPC_VERSION}" \
    && GOBIN=/usr/local/bin go install "connectrpc.com/connect/cmd/protoc-gen-connect-go@v${PROTOC_GEN_CONNECT_GO_VERSION}" \
    && rm -rf /root/go /root/.cache/go-build

USER node
WORKDIR /home/runner/_work

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Rust toolchain: derives from the agent image and adds the Rust toolchain on top.
FROM agent AS rust
USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG RUST_VERSION

ENV RUSTUP_HOME=/usr/local/rustup
ENV CARGO_HOME=/usr/local/cargo
ENV PATH="/usr/local/cargo/bin:/usr/local/rustup/bin:${PATH}"
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain "${RUST_VERSION}" --profile minimal \
    && rustup component add rustfmt clippy \
    && chown -R node:node /usr/local/cargo /usr/local/rustup

ENV PROTOC=/usr/local/bin/protoc

USER node
WORKDIR /home/runner/_work

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Java 21 toolchain: derives from the agent image and adds the JDK 21 + Gradle on top.
FROM agent AS java21
USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG TARGETARCH
ARG JAVA_VERSION=21
ARG GRADLE_VERSION

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

ENV JAVA_HOME=/opt/jdk
ENV PATH="/opt/jdk/bin:${PATH}"

# Gradle binary distribution so the gradle wrapper can resolve locally when offline
RUN curl -fsSL "https://services.gradle.org/distributions/gradle-${GRADLE_VERSION}-bin.zip" -o /tmp/gradle.zip \
    && unzip -q /tmp/gradle.zip -d /opt \
    && ln -s "/opt/gradle-${GRADLE_VERSION}/bin/gradle" /usr/local/bin/gradle \
    && rm -f /tmp/gradle.zip

ENV GRADLE_HOME=/opt/gradle-${GRADLE_VERSION}
ENV PATH="/opt/gradle-${GRADLE_VERSION}/bin:${PATH}"

USER node
WORKDIR /home/runner/_work

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Java 17 toolchain: derives from the agent image and adds the JDK 17 + Gradle on top.
FROM agent AS java17
USER root
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG TARGETARCH
ARG JAVA_VERSION=17
ARG GRADLE_VERSION

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

ENV JAVA_HOME=/opt/jdk
ENV PATH="/opt/jdk/bin:${PATH}"

RUN curl -fsSL "https://services.gradle.org/distributions/gradle-${GRADLE_VERSION}-bin.zip" -o /tmp/gradle.zip \
    && unzip -q /tmp/gradle.zip -d /opt \
    && ln -s "/opt/gradle-${GRADLE_VERSION}/bin/gradle" /usr/local/bin/gradle \
    && rm -f /tmp/gradle.zip

ENV GRADLE_HOME=/opt/gradle-${GRADLE_VERSION}
ENV PATH="/opt/gradle-${GRADLE_VERSION}/bin:${PATH}"

USER node
WORKDIR /home/runner/_work

# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #

# Final stage: the full agent image (default build target for bare docker build).
FROM agent AS final
USER node
WORKDIR /home/runner/_work
