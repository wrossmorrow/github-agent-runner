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
