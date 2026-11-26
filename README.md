# agentrunner

Self-hosted **"agentic" GitHub Actions runners** on Kubernetes using **Actions Runner Controller (ARC) v2** and the [`pi`](https://pi.dev/) coding-agent harness.

Platform admins declare toolchains (`AgenticRunnerToolchain`) and runner profiles (`AgenticRunnerDeployment`) via Kubernetes CRDs—specifying the container image, model, external gateway endpoint, egress allowlists, and base instructions. Repository maintainers target those runners like any standard ARC v2 scale set (`runs-on: <runnerScaleSetName>`) and invoke agentic tasks (such as PR code reviews or test-coverage checks) with a thin composite GitHub Action.

- **ARC v2**: <https://github.com/actions/actions-runner-controller>
- **pi harness**: <https://pi.dev/>

---

## Architecture

An `AgenticRunnerDeployment` compiles into an ARC v2 `AutoscalingRunnerSet` whose pods keep the **stock `ghcr.io/actions/actions-runner` container** unmodified and run the toolchain + security boundary as **Kubernetes 1.29+ native sidecars**:

```text
+-----------------------------------------------------------------------+
| Ephemeral ARC v2 Runner Pod (NetworkPolicy enforced)                  |
|                                                                       |
|  runner (stock) ──localhost──> agent (AgenticRunnerToolchain image)   |
|    actions/checkout   /home/     agentrunnerd + pi --mode rpc         |
|    thin action step  <=====>     reads /etc/agentrunner & /tokens:ro  |
|                      _work         |                |                 |
|                                    v                v                 |
|                            egress-proxy      github-auth              |
|                            (Squid allowlist) (App RBAC -> /tokens)    |
+------------------------------------|----------------|-----------------+
                                     v                v
                        External Model Gateway    GitHub API
```

- **Dedicated GitHub App RBAC**: The `github-auth` sidecar mints short-lived, repo-scoped tokens to a `tmpfs` mounted read-only into `agent` (never into `runner`), bounded by a dedicated review-bot App (`Contents: Read`, `Pull requests: Write`).
- **Per-pod Egress Control**: `egress-proxy` (Squid) + a generated `NetworkPolicy` restrict outbound traffic to GitHub, the configured model gateway, and `AgenticRunnerToolchain.spec.egressAllowlist`.
- **Gateway Separation of Concerns**: `pi` configuration (`models.json`, limits, CI attribution headers, and layered `AGENTS.md`) is rendered per deployment/run and assumes an external Model Gateway / Agent Router owns upstream LLM credentials and usage accounting.

---

## Dependencies

`agentrunner` expects these external pieces in your environment:

- **Kubernetes v1.29+**: Required for native sidecars (`initContainers` with `restartPolicy: Always`). Production clusters should also use a CNI that enforces `NetworkPolicy` (e.g. Cilium, Calico, AWS VPC CNI, GKE Dataplane V2).
- **ARC v2 (`gha-runner-scale-set-controller`)**: Installed in the cluster (default namespace `arc-systems`); `agentrunner` composes ARC v2 `AutoscalingRunnerSet`s rather than replacing ARC.
- **GitHub Apps (two distinct permission roles)**:
  1. **Runner registration (`arc-github-auth`)**: An ARC GitHub App with `Administration: Read & write` (repo-scoped) or `Self-hosted runners: Read & write` (org-scoped)—or a PAT for quick local testing.
  2. **Review bot (`review-bot-app-key`)**: A dedicated GitHub App with **`Contents: Read-only`** and **`Pull requests: Read & write`** so the agent pod cannot push commits even if prompt-injected.
- **Model Gateway / Endpoint**: An OpenAI-compatible (`/v1/chat/completions`) endpoint or AI gateway serving a tool-calling model capable of code review (e.g., an org agent gateway, LiteLLM, or a local server like `flint` for testing).
- **Container Images**: Cluster pull access (or preloaded `kind` images) for stock `ghcr.io/actions/actions-runner:latest`, `ubuntu/squid:latest`, and the `agentrunner-*` images.

---

## Install (Helm OCI)

### 1. Install the Controller & CRDs
```bash
helm upgrade --install agentrunner-controller \
  oci://ghcr.io/wrossmorrow/charts/agentrunner-controller \
  --namespace agentrunner-system \
  --create-namespace
```

*(The controller auto-detects the installed ARC controller version from `arc-systems`; override with `--set controller.arcVersion=<ver>` if needed.)*

### 2. Provision Namespace Secrets & Base Policy
In your runner namespace (e.g. `actions-runners`):

```bash
kubectl create namespace actions-runners

# 1. ARC v2 runner registration auth (PAT or ARC GitHub App)
kubectl -n actions-runners create secret generic arc-github-auth \
  --from-literal=github_token="<ARC_REGISTRATION_TOKEN>"

# 2. Dedicated review-bot GitHub App private key (Contents: Read, Pull requests: Write)
kubectl -n actions-runners create secret generic review-bot-app-key \
  --from-file=private-key.pem=/path/to/review-bot.pem

# 3. Model gateway bearer token
kubectl -n actions-runners create secret generic agent-gateway-token \
  --from-literal=token="<GATEWAY_BEARER_TOKEN>"

# 4. Base read-only reviewer prompt (00-base.md)
kubectl -n actions-runners create configmap org-review-policy \
  --from-literal=00-base.md='You are a read-only code review agent running in CI.
Inspect the repository and diff using your read and git tools. Do not modify files or push commits.
End your final response with a single JSON object matching:
{"verdict":"APPROVE"|"COMMENT"|"REQUEST_CHANGES","summary":"...","comments":[{"path":"...","line":1,"severity":"info"|"warning"|"error","body":"..."}]}'
```

### 3. Apply a Toolchain & Runner Deployment
Edit [examples/toolchain-go.yaml](examples/toolchain-go.yaml) and [examples/deployment-go-reviewer.yaml](examples/deployment-go-reviewer.yaml) with your repository URL, gateway endpoint, and GitHub App/Installation IDs, then apply:

```bash
kubectl apply -f examples/toolchain-go.yaml
kubectl apply -f examples/deployment-go-reviewer.yaml

kubectl get artc
kubectl -n actions-runners get ard
```

---

## Use in a GitHub Actions Workflow

Add a workflow to your target repository referencing the scale-set name (`spec.runner.runnerScaleSetName`) and [`action/`](action/action.yml) (pin a release tag or, for production, a full commit SHA):

```yaml
on: [pull_request]

jobs:
  agent-review:
    runs-on: agentic-go-review
    steps:
      - uses: actions/checkout@v7
        with:
          fetch-depth: 0

      - uses: wrossmorrow/github-agent-runner/action@v0.0.1
        with:
          instructions: |
            Review the diff against ${{ github.base_ref }}.
            Verify any new exported functions have corresponding unit tests.
          fail-on: error
          output: pr-review
```

### Action Inputs

| Input | Default | Description |
| :--- | :--- | :--- |
| `instructions` | *(required)* | Task prompt passed to the agent (e.g. review criteria, check rules) |
| `fail-on` | `error` | Fail the check when any finding meets severity (`none` \| `warning` \| `error`) |
| `output` | `pr-review` | Where to publish results (`pr-review` \| `comment` \| `stdout`) |
| `endpoint` | `http://127.0.0.1:8788/review` | Local `agentrunnerd` sidecar HTTP endpoint |

---

## Published Artifacts

| Artifact | Location |
| :--- | :--- |
| Helm chart (CRDs + controller) | `oci://ghcr.io/wrossmorrow/charts/agentrunner-controller` |
| Controller image | `ghcr.io/wrossmorrow/agentrunner-controller` |
| GitHub App token sidecar | `ghcr.io/wrossmorrow/agentrunner-github-auth` |
| Toolchain sidecar images | `ghcr.io/wrossmorrow/agentrunner-{agent,python,golang,rust,java17,java21}` |

---

## Local Testing & Development

- **Local `kind` walkthrough**: See [docs/KIND_TESTING.md](docs/KIND_TESTING.md) to test locally against `kind` + a host model server (`http://host.docker.internal:11500/v1`).
- **Architecture & design notes**: See [PLAN.md](PLAN.md).

### Common `just` Recipes
```bash
just check                    # gofmt check, go vet, go build, go test -race -shuffle=on
just render-summary           # render built-in example manifests and list Kind/Name
just containerize golang      # build a toolchain image (agent|python|golang|rust|java17|java21)
just containerize-github-auth # build the github-auth sidecar image
just containerize-controller  # build the controller image
just sync-chart-crds          # copy config/crd/*.yaml into charts/agentrunner-controller/crds/
```

### Repository Layout
- `api/v1alpha1/`: `AgenticRunnerToolchain` and `AgenticRunnerDeployment` Go types and validation.
- `pkg/render/`: Pure-Go renderer compiling CRDs into ARC v2 `AutoscalingRunnerSet`, `ConfigMap`, `Role`/`RoleBinding`, and `NetworkPolicy` manifests.
- `pkg/controller/`: `controller-runtime` reconciler watching `AgenticRunnerDeployment` and `AgenticRunnerToolchain`.
- `pkg/pirpc/` & `pkg/review/`: `pi --mode rpc` JSONL client, per-run `models.json` + numbered `AGENTS.md` assembly, `git diff --stat` prompting, verdict parsing, and `gh` PR review poster.
- `cmd/agentrunner-controller/`, `cmd/agentrunnerd/`, `cmd/agentrunner-render/`: Controller manager, sidecar HTTP daemon, and manifest CLI.
- `images/`: Dockerfiles for `controller`, `github-auth`, and multi-stage `toolchains`.
- `charts/agentrunner-controller/`, `config/crd/`, `config/controller/`, `examples/`, `action/`: Helm chart (with `values.schema.json`), CRDs, examples, and composite GitHub Action.

---

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.

---

Built with Antigravity (Google DeepMind) using Gemini 4 Neon.
