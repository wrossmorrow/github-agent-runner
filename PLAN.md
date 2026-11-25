# Agentic Self-Hosted GitHub Runners — Architecture Sketch

## Overview & Scope

### Goal
Run "agentic" GitHub Actions workflows on self-hosted Kubernetes runners using CRD composition over **Actions Runner Controller (ARC) v2**:
- **Platform/Cluster Admins** define runner capabilities, toolchains, `pi` harness configuration, model gateway wiring, egress policies, and guardrails via Kubernetes CRDs.
- **Repository Maintainers** target those runners using standard ARC v2 `runs-on` scale-set names and invoke agentic tasks through a normal-looking GitHub Actions step (`uses: my-org/agent-review@v1`) with task-specific `instructions`.

### Initial v1 Scope
Start with a **read-only PR review & check bot** (e.g., code review, check triage, enforcing that new exported functions or features include tests). Keep execution, `pi` configuration, and GitHub side effects decoupled so the same architecture can expand later to mutating or autofix workflows.

### Core Separation of Concerns (`pi` Config + External Gateway)
The clean boundary between infrastructure and workflow tasks lives in **`pi` configuration** combined with an assumed external **Model Gateway / Agent Router** layer:
- **Cluster/Platform Layer (Admin-Owned)**:
  - **`pi` runtime config**: Generated from `AgenticRunnerDeployment` and `AgenticRunnerToolchain` and mounted read-only into the `agent` sidecar (`$PI_CODING_AGENT_DIR` and `/etc/agentrunner/`). Covers provider/model selection, gateway endpoint, custom attribution headers, enabled extensions, turn/duration limits, and the base `AGENTS.md` layers.
  - **Model Gateway / Agent Router** (e.g., AgentGateway, Envoy AI Gateway, LiteLLM, or an org gateway): Treated as an existing external service that owns upstream provider credentials, model routing, rate limiting, token/cost accounting, and request auditing. The runner pod only needs the gateway endpoint, optional auth/identity, and attribution headers configured via `pi`.
  - **Egress & GitHub Auth Isolation**: Per-pod forward egress proxy sidecar (allowlists + container-only audit logs) and a dedicated GitHub App token sidecar governed by GitHub App RBAC.
- **Workflow / Repo Layer (User-Owned)**:
  - Standard `runs-on` targeting, repository `AGENTS.md`, and step-level `instructions`, `fail-on`, and `output` inputs.

> **Prior Art / Proven Patterns**: This design builds on proven local patterns—a Go `pi --mode rpc` client with optional S3 session archival, multi-stage toolchain container images, a numbered `/opt/agents/*.md` cascade, a forward egress proxy, and an isolated GitHub App token sidecar—generalized so the Kubernetes control plane does not depend on any single local harness or gateway implementation.

---

## Kubernetes CRD Model (Composition)

Rather than inheriting from ARC runner types or stacking custom toolchains onto ARC's runner image, an `AgenticRunnerDeployment` composes an **ARC v2 `AutoscalingRunnerSet`** and references a cluster-scoped `AgenticRunnerToolchain` run as a native sidecar.

### 1. `AgenticRunnerToolchain` (Cluster-Scoped)
Thin, `RuntimeClass`-like resource managed by platform admins. Bundles a prebuilt container image with its resource profile, egress allowlist, and CRD-managed `agentsMd` guidance.

Defining `agentsMd` on the CRD (in addition to or instead of image-baked files) gives two practical advantages:
- **Fast iteration**: Tweak toolchain instructions (e.g., test commands or linter flags) with `kubectl apply` rather than rebuilding and pushing container images.
- **Image portability**: Works cleanly even when referencing base images that do not bake `/opt/agents/10-toolchain.md`.

```yaml
apiVersion: agentic.example.io/v1alpha1
kind: AgenticRunnerToolchain
metadata:
  name: go-1.23
spec:
  image: ghcr.io/org/agentrunner-go:1.23
  resources:
    requests:
      cpu: "2"
      memory: "4Gi"
    limits:
      cpu: "4"
      memory: "8Gi"
  egressAllowlist:
    - proxy.golang.org
    - sum.golang.org
  agentsMd: |
    Toolchain: Go 1.23, golangci-lint, protoc, buf, git, gh.
    Prefer `go test -race ./...` when validating Go changes.
```

### 2. `AgenticRunnerDeployment` (Namespace-Scoped)
Composes an ARC v2 `AutoscalingRunnerSet` (single-label `runs-on: <runnerScaleSetName>`, scale-to-zero, standard `spec.template.spec` PodSpec) and renders the read-only `pi` configuration mounted into the `agent` sidecar.

```yaml
apiVersion: agentic.example.io/v1alpha1
kind: AgenticRunnerDeployment
metadata:
  name: go-reviewer
  namespace: actions-runners
spec:
  # Composes ARC v2 AutoscalingRunnerSet configuration
  runner:
    githubConfigUrl: https://github.com/my-org/my-repo # or org / enterprise
    githubConfigSecret: arc-github-auth
    runnerScaleSetName: agentic-go-review              # targets runs-on: agentic-go-review
    minRunners: 0
    maxRunners: 5

  toolchainRef:
    name: go-1.23

  agent:
    harness: pi
    model: claude-sonnet-4-5
    gateway:
      endpoint: https://agent-gateway.internal/v1
      authSecretRef:
        name: agent-gateway-token
    limits:
      maxTurns: 25
      maxDuration: 10m
      maxCostUsd: 2.00
    systemPromptRef:
      configMapName: org-review-policy
```

Reconciling `AgenticRunnerDeployment` produces:
- An ARC v2 `AutoscalingRunnerSet` whose `spec.template.spec` keeps the stock `runner` container and injects the `agent`, `egress-proxy`, and `github-auth` native sidecars.
- Read-only `ConfigMap`s mounted into the `agent` sidecar (`$PI_CODING_AGENT_DIR` / `/etc/agentrunner/`) containing the rendered `pi` provider/model config, gateway endpoint + attribution config, execution limits, and layered `AGENTS.md` fragments.
- A generated `NetworkPolicy` and per-pod egress proxy configuration.

---

## Pod Topology, Gateway Boundary & Security

Using Kubernetes **native sidecars** (`initContainers` with `restartPolicy: Always`, K8s 1.29+) lets the `runner` container remain **stock `ghcr.io/actions/actions-runner`** while running the unmodified `AgenticRunnerToolchain` image alongside it. Sidecars start before `runner` and terminate cleanly when the ephemeral `runner` container exits.

```text
+-------------------------------------------------------------------------+
| Ephemeral ARC v2 Runner Pod (NetworkPolicy enforced)                    |
|                                                                         |
|  +-----------------------+  localhost HTTP/SSE   +-------------------+  |
|  | runner (main)         | --------------------> | agent (sidecar)   |  |
|  | - stock actions-      |                       | - toolchain image |  |
|  |   runner              |   shared /_work vol   | - agentrunnerd    |  |
|  | - actions/checkout    | <===================> | - pi --mode rpc   |  |
|  | - thin action step    |   (emptyDir + fsGroup)| - /etc/agentrunner|  |
|  +-----------------------+                       +----+---------+----+  |
|                                                       |         |       |
|                                        localhost      |         |       |
|                                        proxy env      |         |/tokens|
|                                                       v         v       |
|                                            +--------------+ +----------+|
|                                            | egress-proxy | |github-auth|
|                                            | (sidecar)    | |(sidecar) ||
|                                            | - allowlist  | |- App RBAC||
|                                            | - pod logs   | |- tmpfs   ||
|                                            +------+-------+ +----+-----+|
+---------------------------------------------------|--------------|------+
                                                    |              |
                                                    v              v
                                            External Model    GitHub API
                                            Gateway / Agent
                                            Router
```

### 1. Stock `runner` + Native `agent` Sidecar
- **`runner` container**: Stock `ghcr.io/actions/actions-runner` (never rebuilt or stacked with language toolchains; upgrades cleanly with ARC v2). Executes `actions/checkout` into the shared `/_work` `emptyDir` volume.
- **`agent` native sidecar**: Runs the unmodified `AgenticRunnerToolchain` image (`golang`, `rust`, `python`, etc.), hosting `pi` and a local HTTP/SSE daemon (`agentrunnerd`) on `127.0.0.1:<port>`. Reads `/_work` via the shared `emptyDir` (with `securityContext.fsGroup` / UID alignment between ARC's `runner` UID 1001 and the toolchain user). Holds no raw GitHub App private key or upstream LLM provider keys.

### 2. External Model Gateway / Agent Router
- Assumed to exist as a shared platform layer (in-cluster or external). Holds upstream LLM provider keys, routes models, enforces quotas/cost caps, and records token/usage telemetry.
- Through `pi` configuration (provider base URL, headers/extensions), requests carry standard attribution headers (e.g., `x-session-id`, `x-github-repo`, `x-github-run-id`, `x-github-pr`) so the gateway can attribute cost and usage per repository, workflow run, and PR.

### 3. Per-Pod `egress-proxy` Sidecar
- Running a per-pod forward proxy native sidecar keeps v1 simple and maps 1:1 to `AgenticRunnerToolchain.spec.egressAllowlist` plus cluster defaults (GitHub API/actions endpoints, model gateway).
- Standard `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, and `JAVA_TOOL_OPTIONS` environment variables on the `agent` sidecar ensure language tools and package managers respect the proxy boundary.
- Proxy audit logs stay in the `egress-proxy` container logs (never dumped to GitHub Actions step stdout).

### 4. Dedicated GitHub App RBAC (`github-auth` Sidecar)
- **Permission ceiling via GitHub App RBAC**: Use a dedicated review-bot GitHub App configured with **`Contents: Read` + `Pull requests: Write`** (separate from any coding/commit App with `Contents: Write`). GitHub's own App RBAC enforces read-only code access at the API boundary.
- The `github-auth` sidecar mints repo-scoped installation tokens into a shared `tmpfs` (`/tokens/token`, mounted into `agent` only, never `runner`), consumed by `gh` via a wrapper shim.
- *(Optional future hardening)*: If cross-PR isolation within the same repository is later required (preventing a prompt-injected PR #10 from commenting on PR #11), `github-auth` can swap `/tokens/token` for a narrow localhost PR-scoped capability endpoint (`POST /v1/pr-review`).

---

## GitHub Actions Integration & `AGENTS.md` / Prompt Layering

### Workflow YAML (Repository-Owned)
From the repository's perspective, the job looks like a standard ARC v2 workflow targeting the scale-set name:

```yaml
on: [pull_request]

jobs:
  agent-review:
    runs-on: agentic-go-review
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - uses: my-org/agent-review@v1
        with:
          instructions: |
            Review the diff against ${{ github.base_ref }}.
            Verify any new exported functions have corresponding unit tests.
          fail-on: error
          output: pr-review
```

### Thin Composite Action (`my-org/agent-review/action.yml`)
Because `agentrunnerd` runs inside the `agent` sidecar, the stock `runner` image needs **no custom CLI or toolchain binaries**. A thin composite action posts the job inputs to localhost, streams GitHub Actions log groups back to stdout, and exits with the daemon's verdict status:

```yaml
name: Agentic Review
inputs:
  instructions:
    description: What the agent should review or check
    required: true
  fail-on:
    description: Fail the step on severity (none | warning | error)
    default: error
  output:
    description: Output target (pr-review | comment | stdout)
    default: pr-review
runs:
  using: composite
  steps:
    - shell: bash
      env:
        AGENT_INSTRUCTIONS: ${{ inputs.instructions }}
        AGENT_FAIL_ON: ${{ inputs.fail-on }}
        AGENT_OUTPUT: ${{ inputs.output }}
      run: |
        jq -n \
          --arg workspace "$GITHUB_WORKSPACE" \
          --arg eventPath "$GITHUB_EVENT_PATH" \
          --arg instructions "$AGENT_INSTRUCTIONS" \
          --arg failOn "$AGENT_FAIL_ON" \
          --arg output "$AGENT_OUTPUT" \
          --arg repo "$GITHUB_REPOSITORY" \
          --arg runId "$GITHUB_RUN_ID" \
          '{workspace:$workspace, eventPath:$eventPath, instructions:$instructions, failOn:$failOn, output:$output, repo:$repo, runId:$runId}' \
        | curl --fail-with-body -sSN -X POST \
            -H 'Content-Type: application/json' \
            --data-binary @- \
            http://127.0.0.1:8788/review
```

### Numbered `AGENTS.md` Cascade & Runtime Prompt Layering
A numbered `/opt/agents/*.md` → `$PI_CODING_AGENT_DIR/AGENTS.md` cascade cleanly separates cluster policy, toolchain facts, repo conventions, and per-step instructions:

| Order / Layer | Source | Owner | Purpose |
| :--- | :--- | :--- | :--- |
| **`00-base.md`** | `AgenticRunnerDeployment` (`systemPromptRef`) | Cluster Admin | Read-only reviewer role, guardrails, and structured JSON output contract |
| **`10-toolchain-image.md`** | Container image `/opt/agents/10-toolchain.md` (optional) | Image Maintainer | Baked-in image facts, if present |
| **`15-toolchain-crd.md`** | `AgenticRunnerToolchain.spec.agentsMd` | Platform Admin | Hot-updatable toolchain hints (compilers, test flags, linters) |
| **`20-deployment.md`** | `AgenticRunnerDeployment` (optional) | Deployment Admin | Repo/org-specific deployment overrides |
| **Runtime step context** | `$GITHUB_WORKSPACE/AGENTS.md` + `AGENT_INSTRUCTIONS` + `$GITHUB_EVENT_PATH` + `git diff --stat` | Repo Maintainers & Workflow | Repo conventions, task prompt, PR metadata, base/head SHAs |

---

## Runtime Execution Flow (`runner` → `agentrunnerd` ↔ `pi` RPC)

1. **Receive step request**: `agentrunnerd` (inside the `agent` sidecar) accepts `POST /review`, reads the rendered `pi` config and `AGENTS.md` layers, `$GITHUB_EVENT_PATH` (via shared `/_work` or payload), and optional `$GITHUB_WORKSPACE/AGENTS.md`.
2. **Summarize diff without stuffing context**: Computes `git diff --stat` and changed file paths against the PR base/head SHAs rather than embedding the entire diff into the prompt; `pi` uses its `read` and `git` tools to inspect hunks and files on demand (handling large diffs naturally).
3. **Spawn `pi` in RPC mode**: Uses a Go `pi --mode rpc` JSONL client configured to talk to the Model Gateway / Agent Router with CI attribution headers (`x-session-id`, `x-github-repo`, `x-github-run-id`, `x-github-pr`), optionally archiving session logs (`parts/*.jsonl.gz`, `metadata.json`, `_closed`) to object storage (e.g., S3).
4. **Stream & supervise**: Maps streaming RPC events (tool calls, turns) into GitHub Actions log groups (`::group::` / `::endgroup::`) streamed back to the `runner` step stdout while enforcing `maxTurns` and `maxDuration`.
5. **Collect structured output**: Requires `pi` to emit a structured review payload:
   ```json
   {
     "verdict": "REQUEST_CHANGES",
     "summary": "Missing unit tests for new token parser.",
     "comments": [
       {
         "path": "pkg/auth/token.go",
         "line": 42,
         "severity": "error",
         "body": "..."
       }
     ]
   }
   ```
6. **Post via GitHub App token & exit**: When `output` is `pr-review` or `comment`, `agentrunnerd` posts the review via `gh` using the App-RBAC-constrained `/tokens/token` from `github-auth`, and returns a non-zero status if any finding meets or exceeds `failOn`.

---

## Remaining Open Questions & Next Steps

1. **Concrete ARC v2 PodSpec & volume permissions**: Verify exact `AutoscalingRunnerSet` `spec.template.spec` merging behavior in ARC v2 and align `/_work` permissions (`securityContext.fsGroup` / UIDs) between stock `actions-runner` (UID 1001) and toolchain containers.
2. **`agentrunnerd` HTTP/SSE contract & `pi` RPC output schema**: Define the exact `POST /review` request/streaming-response protocol (including heartbeat/exit-code framing over HTTP) and the `pi` structured review JSON schema / final-tool contract.
3. **Optional PR-scoped capability proxy (deferred)**: Keep `github-auth` simple with dedicated App RBAC + `/tokens/token` for v1; revisit a PR-scoped `POST /v1/pr-review` capability proxy if cross-PR isolation within the same repo becomes a requirement.
4. **Future mutating capability tiers (deferred)**: Define how later iterations opt into mutating workflows (e.g., autofix commits or PR branches) with separate `Contents: Write` GitHub Apps and stricter repo/branch guardrails.
