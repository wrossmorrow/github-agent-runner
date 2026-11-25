# agentrunner

Skeleton implementation of CRD-composed agentic self-hosted GitHub Actions runners on **ARC v2** (`AutoscalingRunnerSet`) using **Kubernetes native sidecars** and `pi --mode rpc`. See [PLAN.md](PLAN.md) for the architecture sketch and [docs/KIND_TESTING.md](docs/KIND_TESTING.md) for a local `kind` testing walkthrough.

* ARC v2: https://github.com/actions/actions-runner-controller
* pi harness: https://pi.dev/

## Layout

- `api/v1alpha1/`: `AgenticRunnerToolchain` and `AgenticRunnerDeployment` CRD types and validation.
- `pkg/render/`: Pure-Go renderer compiling the CRDs into ARC v2 `AutoscalingRunnerSet`, `ConfigMap`s (`/etc/agentrunner/`, `squid.conf`), and `NetworkPolicy` manifests.
- `pkg/controller/`: `controller-runtime` reconciler watching `AgenticRunnerDeployment` and `AgenticRunnerToolchain` and applying rendered bundles via Server-Side Apply.
- `pkg/pirpc/`: JSONL RPC client for `pi --mode rpc`.
- `pkg/review/`: Numbered `AGENTS.md` cascade, `git diff --stat` prompt assembly, structured JSON verdict extraction, `gh` PR review poster, and review execution loop.
- `cmd/agentrunner-controller/`: Kubernetes controller manager binary.
- `cmd/agentrunnerd/`: HTTP/streaming daemon running inside the `agent` native sidecar (`POST /review`, `GET /healthz`).
- `cmd/agentrunner-render/`: CLI to render manifests (`--example` or JSON files).
- `action/action.yml`: Thin composite GitHub Action (`curl` + `jq`) for stock ARC runner containers.
- `config/crd/`, `config/controller/` & `examples/`: Kubernetes CRD manifests, controller RBAC/Deployment, and sample YAML definitions.

## Quick Verification

```bash
go test -race ./...
go run ./cmd/agentrunner-render --example | jq -r '.[] | "\(.kind)/\(.metadata.name)"'
```

---

Built with Antigravity (Google DeepMind) using Gemini 4 Neon.
