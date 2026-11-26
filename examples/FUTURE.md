# Future Runner Types & Autonomous PR Loops

`gitub-agent-runner` currently implements a single **read-only PR review & check bot** (`POST /review`). Because the architecture cleanly separates **compute/toolchains** (`AgenticRunnerToolchain`), **identity & permissions** (dedicated GitHub App per `AgenticRunnerDeployment`), **egress** (`egress-proxy` + `NetworkPolicy`), **model policy** (external Model Gateway / Agent Router), and **task instructions** (`00-base.md` + GitHub Action inputs), the same foundation generalizes across a wide spectrum of CI/CD agents.

This document sketches:
1. Additional runner types arranged along a **permission ladder** (Tiers 1–3).
2. A north-star sketch of an **autonomous loop** (create → review → fix → verify → approve) built from separate, bounded runners.
3. Exact **GitHub App permissions** required for each role.
4. What is currently **missing** across CRDs, controller, `agentrunnerd`/`action`, `github-auth`, and the gateway to build them safely.

---

## 1. Permission Ladder & Candidate Runner Types

### Tier 1 — Read-Only / Advisory (`Contents: Read`)
Low-risk agents that inspect code or logs inside an ephemeral toolchain container and post comments, statuses, or draft releases without write access to repository contents.

#### A. CI Failure Triage Bot
- **Example sketch (not yet implemented)**: [examples/future/ci-triage.yml](future/ci-triage.yml)
- **Trigger**: `on: workflow_run` (types: `[completed]`, filtered to `conclusion == 'failure'`) or a failed-step fallback job.
- **How it works**: Fetches the failing job logs, checks out the PR commit inside the matching `AgenticRunnerToolchain` container, re-runs the failing command locally (`go test`, `pytest`, `cargo test`), distinguishes real regressions from flakes/infra errors, and posts a concise root-cause comment on the PR.
- **Output**: `pr-comment` or `check-run` summary.

#### B. Issue Triage & Minimal Reproducer Bot
- **Example sketch (not yet implemented)**: [examples/future/issue-triage.yml](future/issue-triage.yml)
- **Trigger**: `on: issues` (`types: [opened, labeled]`).
- **How it works**: Reads the issue body, searches the codebase, attempts to write and run a temporary reproduction test inside the sandboxed container (never committed), and comments with suspected files, reproduction result (`reproduced` / `not reproduced`), and suggested labels.
- **Output**: `issue-comment` + optional label application.

#### C. Specialized Policy & Audit Gates
- **Example sketch (not yet implemented)**: [examples/future/policy-audit.yml](future/policy-audit.yml)
- **Trigger**: `on: pull_request` or `on: schedule`.
- **Examples**:
  - Breaking API / Protobuf / SQL migration reviewer.
  - Security / auth-boundary / RBAC diff auditor.
  - Doc-drift checker (*"public CLI flags changed without updating README/docs"*).
  - Scheduled CVE reachability audit (*"is this dependency vulnerability reachable from our codebase?"*).
- **Output**: `pr-review` or `sarif` / `check-run` annotations.

#### D. Release Notes Drafter
- **Example sketch (not yet implemented)**: [examples/future/release-notes.yml](future/release-notes.yml)
- **Trigger**: `on: push` (`tags: ['v*.*.*']`) or `workflow_dispatch`.
- **How it works**: Inspects the full diff and PR metadata between the previous tag and the new tag (rather than just commit titles), verifies breaking changes or new CRD fields, and creates or updates a **draft** GitHub Release.
- **Output**: `release-draft`.

#### E. Deployment & Canary Verifier
- **Example sketch (not yet implemented)**: [examples/future/deploy-verify.yml](future/deploy-verify.yml) (job `verify-deployment`)
- **Trigger**: `on: deployment_status` (filtered to `state == 'success'`) or a post-deploy `workflow_run`.
- **How it works**: Runs smoke checks against `deployment_status.environment_url`, inspects read-only runtime telemetry (pod restarts, Kubernetes events, recent error logs, Prometheus/OTel metrics via a narrowly scoped read-only `ServiceAccount` / Workload Identity and allowlisted egress), correlates anomalies against the exact deployed commit diff, and emits a pass/fail verdict.
- **Output**: `deployment-status` (`success` / `failure`), commit/PR comment, or incident issue.

---

### Tier 2 — One-Click Suggestions (Still `Contents: Read`)

#### F. Suggestion-Block Reviewer
- **Example sketch (not yet implemented)**: [examples/future/suggest-fixes.yml](future/suggest-fixes.yml)
- **Trigger**: `on: pull_request` or `on: issue_comment` (`/suggest`).
- **How it works**: Runs linters/tests locally to verify a small fix (docstrings, missing unit test stubs, lint/vet fixes, typo corrections) and emits GitHub's native ```` ```suggestion ```` markdown blocks in inline PR review comments. A human maintainer clicks **Commit suggestion**—zero `Contents: Write` token exposure in the runner pod.

---

### Tier 3 — Mutating / Branch-Writing (`Contents: Write`, Separate App)
Mutating runners require a **separate GitHub App**, **actor allowlists** (only triggered by maintainers or trusted automation), and **branch protection** preventing direct pushes to `main`.

#### G. Comment-Triggered PR Fixer (`@bot fix` / `/add-tests`)
- **Example sketch (not yet implemented)**: [examples/future/pr-fixer.yml](future/pr-fixer.yml)
- **Trigger**: `on: issue_comment` (`created`, gated by `author_association` / maintainer list).
- **How it works**: Checks out the PR head branch, edits files to address review comments or add missing unit tests, runs the repo's validation suite (`just check`, `go test -race ./...`), commits, and pushes to the PR branch.
- **Guardrail**: Refuses to push to protected/default branches and refuses to modify `.github/workflows/` or CI policy files.

#### H. Scheduled Maintenance & Upgrade PRs
- **Trigger**: `on: schedule` or `workflow_dispatch`.
- **How it works**: Performs multi-step upgrades that Dependabot/Renovate cannot complete (e.g., bumping a major Go, Kubernetes `client-go`, or SDK version, fixing resulting compile/API breaks, running tests), opens a PR on a dedicated `bot/...` branch, and leaves merging to standard CI + human or policy-gated review.

#### I. Automated Revert PR & Rollback Trigger
- **Example sketch (not yet implemented)**: [examples/future/deploy-verify.yml](future/deploy-verify.yml) (job `open-revert-pr`)
- **Trigger**: Downstream job when the Tier 1 Deployment Verifier reports `failure` (or `workflow_dispatch` by an on-call maintainer).
- **How it works**: Two blast-radius-bounded patterns:
  1. **Revert PR (GitOps-friendly)**: Checks out the deployed SHA, runs `git revert --no-edit <sha>` on a `bot/revert-<short-sha>` branch, runs fast unit/build checks to verify the revert compiles cleanly, and opens a revert PR populated with the failing smoke output, error logs, and suspected regression diff. Never force-pushes `main`.
  2. **Deterministic Rollback Trigger (optional, tighter guardrails)**: Rather than granting the agent cluster-admin or direct `kubectl`/`helm` write credentials, dispatches a pre-baked deterministic rollback workflow (`workflow_dispatch` to `rollback.yml`) or calls a narrow capability-proxy endpoint (e.g., ArgoCD/Flux sync to the previous known-good revision).
- **Guardrails**: Cooldown / oscillation lock (prevents deploy → fail → rollback → re-deploy loops), per-environment policy (e.g., auto-rollback allowed in `staging`, revert-PR-only in `production`), and a separate GitHub App from the Tier 1 verifier.

---

## 2. North-Star Sketch: Autonomous Loop

- **Example sketch (not yet implemented)**: [examples/future/auto-loop.yml](future/auto-loop.yml)

A full autonomous PR lifecycle (trigger → create PR → run CI & review → fix issues → re-verify → approve) can be composed cleanly on `gitub-agent-runner` **without a monolithic god-agent**, by splitting roles across separate `AgenticRunnerDeployment`s and separate GitHub Apps (**separation of powers**):

```text
  Issue / Spec / Dispatch
            │
            ▼
  +--------------------+     opens PR      +-----------------------------+
  | author-runner      | ----------------> | GitHub Pull Request         |
  | App: author-bot    |                   | (branch: agent/auto-123)    |
  | (Contents: Write)  |                   +------+---------------+------+
  +--------------------+                          |               |
            ▲                                     │               │
            │  REQUEST_CHANGES                    ▼               ▼
            │  (iteration < max)         +---------------+ +--------------+
  +--------------------+                 | Standard CI   | | review-runner|
  | fixer-runner       | <-------------- | (tests/lint)  | | App: rev-bot |
  | App: author-bot    |   fix trigger   +-------+-------+ | (Read-only!) |
  | (Contents: Write)  |                         |         +------+-------+
  +--------------------+                         │                │
                                                 ▼                ▼
                                           All Checks Green + APPROVE
                                                 │
                                                 ▼
                                     Human Merge (or Auto-Merge Queue)
```

### Why Separate Apps & Runners Matter Here
1. **No self-approval**: GitHub enforces that an App/user cannot approve its own PR—and separating `author-bot` (`Contents: Write`, `Pull requests: Write`) from `reviewer-bot` (`Contents: Read`, `Pull requests: Write`) guarantees the reviewer cannot secretly edit code or CI files to make tests pass.
2. **Independent toolchains & models**: The author/fixer deployment can use a larger reasoning model + full build cache, while fast policy checks use a smaller/cheaper model.
3. **Loop & blast-radius control**:
   - **Per-PR iteration cap** (e.g. max 3 fix cycles tracked via a PR label like `agent-iter:2` or commit count from `author-bot`), failing closed for human takeover once exceeded.
   - **Workflow actor filtering**: Fixing only triggers from `review-runner` completion (`workflow_run` or explicit `/fix` dispatch), never blindly on every `pull_request` push (preventing infinite push/fix loops).
   - **Hard branch & path protection**: `author-bot` is blocked from pushing to `main` and modifying `.github/workflows/**`.
   - **Merge queue / human gate**: Even if `reviewer-bot` emits `APPROVE`, repository branch protection can require a human owner or strict CI status checks before merge.

---

## 3. GitHub App Permissions Matrix

| Runner Role | GitHub App | `Contents` | `Pull requests` | `Issues` | `Actions` | `Checks` | `Deployments` | `Administration` / `Runners` | Notes |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **ARC v2 Controller** | `arc-github-auth` | — | — | — | — | — | — | `Administration: R/W` *(repo)* or `Self-hosted runners: R/W` *(org)* | Used only by ARC controller, never mounted in runner pods |
| **PR Reviewer / Suggester** *(current v1)* | `review-bot` | `Read` | `Read & write` | — | — | `Read` *(optional)* | — | — | Cannot push commits or edit workflows |
| **CI Failure Triage** | `triage-bot` | `Read` | `Read & write` | — | `Read` | `Read` | — | — | Needs `Actions: Read` to download failing workflow logs |
| **Issue Triage / Repro** | `issue-bot` | `Read` | — | `Read & write` | — | — | — | — | Comments & labels on issues only |
| **Release Notes Drafter** | `release-bot` | `Read & write` *(releases only)* or `Read` + draft API | `Read` | — | — | — | — | — | Creates **draft** releases only |
| **Deployment / Canary Verifier** | `deploy-verify-bot` | `Read` | `Read` | `Read & write` *(optional)* | — | — | `Read & write` | — | Read-only staging/metrics access via K8s `ServiceAccount` / Workload Identity; posts deployment status |
| **Author / PR Fixer** *(Tier 3)* | `author-bot` | `Read & write` | `Read & write` | `Read` | — | `Read` | — | — | Separate App; blocked from `main` & `.github/workflows/**` |
| **Revert PR / Rollback Trigger** *(Tier 3)* | `rollback-bot` | `Read & write` | `Read & write` | `Read` | `Read & write` *(optional, rollback dispatch)* | — | `Read` | — | Separate App; opens revert PRs or dispatches deterministic rollback workflow; never force-pushes `main` or holds cluster-admin |

---

## 4. Missing Pieces to Get There

### A. CRDs & Controller (`api/v1alpha1/`, `pkg/render/`, `pkg/controller/`)
- **Task / capability modes** *(implemented in CRD & renderer; non-`read-only` runtime modes currently fail closed with `501` in `agentrunnerd` until Section B/C land)*: Declare allowed agent capabilities on `AgenticRunnerDeployment` (`spec.agent.mode: read-only | suggest | write-branch`) rather than assuming every deployment is a PR reviewer.
- **Rate limits & loop budgets**: CRD fields for `limits.maxIterationsPerPR`, `limits.cooldownSeconds`, and per-deployment concurrency/budget metadata forwarded to the gateway and `agentrunnerd`. (`limits.maxTransportRetries` is implemented for transient mid-stream errors.)
- **Workspace & git write permissions**: For Tier 3 runners, align `/home/runner/_work` write/git-push ergonomics cleanly with `runner` UID `1001` and `agent` UID `1000` (today `agent` only needs read + `git diff` access).
- **Workload Identity / read-only `ServiceAccount`** *(`spec.runner.serviceAccountName` implemented in CRD & renderer)*: Allows deployment pods (including ARC v2 runner/sidecar pods via `AutoscalingRunnerSet.spec.template.spec.serviceAccountName`) to hold narrowly scoped read-only Kubernetes/observability access via cloud Workload Identity without static secrets in GitHub Actions.
- **Session archival config**: Optional S3/GCS bucket & prefix settings on `AgenticRunnerDeployment` so `agentrunnerd` persists JSONL transcripts and `metadata.json` for post-mortem debugging of multi-step runs.
- **Per-deployment egress additions** *(`spec.agent.egressAllowlist` implemented in CRD & renderer)*: Appends deployment-specific domains (e.g., internal artifact registries, staging URLs, or issue trackers) on top of `AgenticRunnerToolchain.spec.egressAllowlist`.

### B. `agentrunnerd` & GitHub Action (`cmd/agentrunnerd/`, `pkg/review/`, `action/`)
- **Generalized task endpoints/modes**: `POST /run` (with backwards-compatible `POST /review` alias) and multi-event hydration (`pull_request`, `issues`, `issue_comment`, `workflow_run`, `push` tags, `deployment_status`) are implemented alongside `pr-review`, `comment`, `issue-comment`, and `release-draft` publishers. Remaining planned outputs (`deployment-status`, `branch-commit`, `revert-pr`, `suggest`) return `501 Not Implemented`.
- **Event hydration beyond `pull_request`**: Hydrate `deployment_status` payloads (resolving environment name, `environment_url`, target SHA, and previous successful deployment SHA) alongside `workflow_run` and `issues`.
- **Git safety guardrails inside `agentrunnerd`**: Before committing/pushing in Tier 3 mode, reject dirty/unexpected target branches (never push to `main`/`master`) and reject diffs touching `.github/workflows/` or policy files.
- **Richer Action outputs (`action/`)**: Upgrade `action/action.yml` from bash `curl` + `jq` to a small zero-dependency Node script (`index.mjs`) that writes structured `$GITHUB_OUTPUT` values (`verdict`, `summary`, `findings`, `pr-number`, `commit-sha`) and Markdown `$GITHUB_STEP_SUMMARY` tables cleanly.

### C. `github-auth` Sidecar (`images/github-auth/`)
- **From `/tokens/token` to a capability proxy for Tier 3**:
  - For Tier 1/2 read-only bots, a downscoped `/tokens/token` (`Contents: Read`) governed by App RBAC is simple and effective.
  - For Tier 3 mutating bots (`Contents: Write`), a prompt-injected PR holding a raw `/tokens/token` could push to *any* unprotected branch in the repo. Replacing `/tokens/token` with a **narrow localhost capability proxy** (`POST http://127.0.0.1:8081/v1/...`) lets the sidecar enforce that pushes target only the bound `agent/...` / PR branch and never touch `.github/workflows/`.
  - For rollback triggers specifically, the capability proxy can restrict mutations to `git revert` PR creation or dispatching a pinned `rollback.yml` workflow ID, paired with cooldown/oscillation checks.
- **Dynamic permission downscoping**: Pass `permissions` explicitly in `github-token.sh`'s `POST /access_tokens` payload as a second defense layer beneath the App's configured RBAC.

### D. Model Gateway / Agent Router (External Platform Layer)
Autonomous multi-agent loops (especially autonomous-style create → review → fix cycles) amplify token burn and runaway-loop risk. The external Model Gateway / Agent Router should enforce:
- **Per-repo / per-PR / per-hour rate limits**: Keyed on the `x-github-repo`, `x-github-pr`, and `x-github-run-id` headers already emitted by `gitub-agent-runner` (`pkg/review/piconfig.go`).
- **Hard token & USD spend ceilings**: Reject calls with `429` once a PR or daily repo budget is exhausted so a misbehaving fix loop fails fast rather than spinning overnight.
- **Model tier routing & fallback**: Route complex author/fixer runs to frontier reasoning models, route fast policy gates to smaller/cheaper models, and fall back gracefully on upstream `429`/`5xx`.
- **Prefix-cache sticky routing**: Hash on `x-github-run-id` / `x-session-id` (or use `InferencePool` EPP for self-hosted GPU pools) so multi-turn agent loops keep warm KV-cache hits.
