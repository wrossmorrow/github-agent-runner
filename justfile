image_repository := "ghcr.io/wrossmorrow"
image_name_prefix := "agentrunner"

# list targets and exit
default:
    just --list

# format all Go files
fmt:
    gofmt -w .

# fail if any Go files are not formatted
fmt-check:
    test -z "$(gofmt -l .)"

# run go vet across all packages
vet:
    go vet ./...

# build all packages and binaries
build:
    go build ./...

# run all tests with race detection and shuffling
test:
    go test -race -shuffle=on -count=1 ./...

# run all tests verbosely
test-v:
    go test -race -shuffle=on -count=1 -v ./...

alias validate := check

# run all formatting, static, build, and test validations
check: fmt-check vet build test

# render the built-in Go reviewer example manifests as JSON
render-example:
    go run ./cmd/agentrunner-render --example | jq .

# print kind/name summary for the rendered Go reviewer example
render-summary:
    go run ./cmd/agentrunner-render --example | jq -r '.[] | "\(.kind)/\(.metadata.name)"'

# build a toolchain sidecar image stage (agent, python, golang, rust, java); minor tags auto-apply matrix build args
containerize toolchain="agent" tag="latest" *args="":
    #!/usr/bin/env bash
    set -euo pipefail
    build_args=()
    while IFS= read -r arg; do
        [ -n "${arg}" ] && build_args+=(--build-arg "${arg}")
    done < <(jq -r --arg name "{{ toolchain }}" --arg minor "{{ tag }}" \
        '.[] | select(.name == $name and .minor == $minor) | .build_args[]?' \
        images/toolchains/matrix.json)
    docker build -f images/toolchains/Dockerfile \
        --target "{{ toolchain }}" \
        "${build_args[@]+"${build_args[@]}"}" \
        {{ args }} \
        -t "{{ image_repository }}/{{ image_name_prefix }}-{{ toolchain }}:{{ tag }}" .

# build the github-auth token-minter sidecar image
containerize-github-auth tag="latest":
    docker build -f images/github-auth/Dockerfile \
        -t {{ image_repository }}/{{ image_name_prefix }}-github-auth:{{ tag }} \
        images/github-auth

# build the Kubernetes controller image
containerize-controller tag="latest":
    docker build -f images/controller/Dockerfile \
        -t {{ image_repository }}/{{ image_name_prefix }}-controller:{{ tag }} .

# run formatting, static, build, and test checks for the optional usage-sink add-on
check-usage-sink:
    cd addons/usage-sink && test -z "$(gofmt -l .)" && go vet ./... && go build ./... && go test -race -shuffle=on -count=1 ./...

# build the optional Envoy ALS usage-sink add-on image
containerize-usage-sink tag="latest":
    docker build -f addons/usage-sink/Dockerfile \
        -t {{ image_repository }}/{{ image_name_prefix }}-usage-sink:{{ tag }} \
        addons/usage-sink

# copy canonical CRDs into the Helm chart crds/ directory
sync-chart-crds:
    cp config/crd/agenticrunnerdeployments.yaml charts/agentrunner-controller/crds/agenticrunnerdeployments.yaml
    cp config/crd/agenticrunnertoolchains.yaml charts/agentrunner-controller/crds/agenticrunnertoolchains.yaml
    cp config/crd/agenticrunnermcpservers.yaml charts/agentrunner-controller/crds/agenticrunnermcpservers.yaml
