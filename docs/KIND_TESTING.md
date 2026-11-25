# Local Testing with `kind`

This guide walks through standing up a local **Kubernetes in Docker (`kind`)** cluster, installing **ARC v2**, loading locally built `agent` and `github-auth` sidecar images, and pointing the agent at a locally running model server (such as `flint` on `host.docker.internal`).

> **Why `kind` works cleanly with ARC v2**: ARC v2 uses outbound long-polling to GitHub, so no public IP, LoadBalancer, or incoming webhook tunnel is needed.
>
> **Note on `NetworkPolicy`**: `kind`'s default CNI (`kindnet`) does not enforce `NetworkPolicy` resources. The `egress-proxy` Squid sidecar still enforces domain allowlists inside the pod via `HTTP_PROXY`/`HTTPS_PROXY`, while `NetworkPolicy` enforcement applies when deploying to EKS, GKE, or a Cilium/Calico cluster.

---

## 1. Prerequisites

- Docker Desktop / OrbStack (must provide `host.docker.internal` from inside containers)
- `kind` (Kubernetes v1.29+ for native `initContainers` `restartPolicy: Always` sidecars — modern `kind` ships v1.31+)
- `kubectl`, `helm`, `just`, `jq`
- A scratch GitHub repository you can open a test PR against

---

## 2. GitHub App Setup (Two Roles)

For local testing you need **two** credentials in Kubernetes:

### A. ARC v2 Runner Registration (`arc-github-auth`)
ARC's controller needs permission to register ephemeral runners on your test repo (or org).

- **Quickest local option (PAT)**: Create a fine-grained Personal Access Token scoped to your test repo with **Administration: Read and write** (and **Metadata: Read-only**).
- **Recommended / prod option (GitHub App)**: Create a GitHub App with **Administration: Read & write** (repo) or **Self-hosted runners: Read & write** (org), install it on your test repo, and download its `.p185822625em` key.

### B. Dedicated Review Bot App (`review-bot-app-key`)
Used solely by the `github-auth` sidecar so the agent pod itself is bounded by GitHub App RBAC:

1. Create a GitHub App (e.g. `my-agentic-reviewer`) with **only**:
   - **Repository permissions → Contents**: `Read-only`
   - **Repository permissions → Pull requests**: `Read and write`
   - **Repository permissions → Metadata**: `Read-only` (auto-selected)
2. Generate and download its private key (`review-bot.pem`).
3. Install the App on your scratch test repository and note:
   - **App ID** (on the App's General page)
   - **Installation ID** (the numeric `<id>` in `https://github.com/settings/installations/<id>`)

---

## 3. Local Model Setup (`flint` or Local OpenAI-Compatible Gateway)

If `flint` (or another local model gateway / `llama-server` / LiteLLM) is running on your host machine on port `11500`:

1. Make sure the host service listens on `0.0.0.0:11500` (or is reachable via `host.docker.internal:11500` from Docker containers — Docker Desktop / OrbStack routes `host.docker.internal` to the host automatically).
2. Pass `-gateway http://host.docker.internal:11500/v1` to `agentrunner-render`.
   - `agentrunner-render` automatically adds `host.docker.internal` to Squid's `allowed_domains` ACL and port `11500` to the `NetworkPolicy`.

Quick host sanity check before deploying:
```bash
# Unauthenticated local server
curl -fsS http://127.0.0.1:11500/v1/models | jq .

# Or authenticated `rec` server
curl -fsS -H "Authorization: Bearer $(rec auth token)" http://127.0.0.1:11500/v1/models | jq .
```

---

## 4. Create the `kind` Cluster & Install ARC v2

```bash
kind create cluster --name agentrunner-dev

# Install the ARC v2 scale-set controller
helm install arc \
  --namespace arc-systems \
  --create-namespace \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set-controller
```

Verify the controller pod is running:
```bash
kubectl -n arc-systems get pods
```

---

## 5. Build & Load Local Images into `kind`

Build the Go toolchain sidecar image and the `github-auth` token-minter image, then load both directly into `kind` (no registry push needed):

```bash
just containerize golang
just containerize-github-auth

kind load docker-image agentrunner-golang:latest --name agentrunner-dev
kind load docker-image agentrunner-github-auth:latest --name agentrunner-dev
```

---

## 6. Create Namespace, Secrets & Base Prompt ConfigMap

```bash
kubectl create namespace actions-runners

# 1. ARC runner registration secret (PAT example; for an ARC GitHub App, pass
#    --from-literal=github_app_id=... --from-literal=github_app_installation_id=... --from-file=github_app_private_key=...)
kubectl -n actions-runners create secret generic arc-github-auth \
  --from-literal=github_token="<YOUR_RUNNER_REGISTRATION_PAT>"

# 2. Review-bot GitHub App private key (mounted into github-auth at /run/secrets/gh-app/private-key.pem)
kubectl -n actions-runners create secret generic review-bot-app-key \
  --from-file=private-key.pem=/path/to/review-bot.pem

# 3. Model gateway token (pass $(rec auth token) for authenticated rec, or "local" if unauthenticated)
kubectl -n actions-runners create secret generic agent-gateway-token \
  --from-literal=token="$(rec auth token)"

# 4. Base read-only reviewer prompt (mounted as /etc/agentrunner/prompts/00-base.md)
kubectl -n actions-runners create configmap org-review-policy \
  --from-literal=00-base.md='You are a read-only code review agent running in CI.
Inspect the repository and diff using your read and git tools. Do not modify files or push commits.
End your final response with a single JSON object matching:
{"verdict":"APPROVE"|"COMMENT"|"REQUEST_CHANGES","summary":"...","comments":[{"path":"...","line":1,"severity":"info"|"warning"|"error","body":"..."}]}'
```

---

## 7. Render & Apply the Runner Deployment

Use `agentrunner-render --example -list` with overrides for your test repo, local `host.docker.internal` gateway, loaded images, and App IDs (note `-arc-version`, defaulting to `0.15.1`, must match your installed ARC controller version or ARC will delete the `AutoscalingRunnerSet`):

```bash
go run ./cmd/agentrunner-render --example -list \
  -repo-url "https://github.com/<owner>/<test-repo>" \
  -gateway "http://host.docker.internal:11500/v1" \
  -model "flint" \
  -image "agentrunner-golang:latest" \
  -github-auth-image "agentrunner-github-auth:latest" \
  -github-app-id "<REVIEW_BOT_APP_ID>" \
  -github-install-id "<REVIEW_BOT_INSTALLATION_ID>" \
  | kubectl apply -f -
```

Verify the `AutoscalingRunnerSet` and listener pod come up:
```bash
kubectl -n actions-runners get autoscalingrunnersets
kubectl -n arc-systems get pods
```

---

## 8. Trigger a Real Workflow Run

In `<owner>/<test-repo>`, add `.github/workflows/agent-review.yml` (referencing the `action/` composite action—either copied into your org/repo or tested via a repo holding `action/action.yml`):

```yaml
on: [pull_request]

jobs:
  agent-review:
    runs-on: agentic-go-review
    steps:
      - uses: actions/checkout@v7
        with:
          fetch-depth: 0

      - uses: <owner>/<repo-hosting-action>@main
        with:
          instructions: |
            Review the diff against ${{ github.base_ref }}.
            Verify any new exported functions have corresponding unit tests.
          fail-on: error
          output: pr-review
```

Open a PR on `<owner>/<test-repo>`, then watch the ephemeral runner pod and all four containers:

```bash
kubectl -n actions-runners get pods -w
```

Once a runner pod is running (`<pod>`):
```bash
kubectl -n actions-runners logs <pod> -c github-auth
kubectl -n actions-runners logs <pod> -c egress-proxy
kubectl -n actions-runners logs <pod> -c agent -f
kubectl -n actions-runners logs <pod> -c runner -f
```

---

## 9. Cleanup

```bash
kind delete cluster --name agentrunner-dev
```
