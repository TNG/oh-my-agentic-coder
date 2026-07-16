# Harness compatibility tracking

omac supports several inner harnesses (opencode, claude-code, codex, copilot, pi).
A harness release can silently break omac by renaming a CLI flag, moving a
subcommand, or changing a config schema. The nightly
[E2E smoke workflow](../.github/workflows/e2e-smoke.yml) is the tripwire: it
installs every harness's **latest** release and records three stages per harness.

| Stage | What it proves | Model call? |
|-------|----------------|-------------|
| `contract` | Every CLI flag/subcommand omac derives from its registry still exists in the harness's `--help` (see `internal/e2e/contract.go`). | no |
| `launch` | `omac start <harness>` reaches the real binary through the sandbox (PATH, config-home, sandbox admission). | no |
| `llm` | A real agent turn against the model provider — verifies the auth/proxy path. Skipped nightly for claude-code (billed externally; runs weekly via [`e2e.yml`](../.github/workflows/e2e.yml)). | yes |

`✅` pass · `❌` fail · `➖` not run.

## Where the record lives

The matrix is **not** committed to this repo (no bot pushes to `main`):

- **Live dashboard** — a single tracking issue titled *"Harness compatibility matrix"*
  (label `auto-update tracker`), rewritten each run with a rolling 30-day window,
  newest first. This is the at-a-glance status.
- **Permanent archive** — every run appends to `harness-compat/HARNESS_COMPAT.md`
  in the private security repo `nhuelstng/oh-my-agentic-coder-security` (a distinct
  subpath from the security scans' `scans/`), giving full diffable history.
- **Alerts** — a `❌` opens/updates a deduplicated drift issue (label
  `harness-drift`, auto-closes when green again) and posts to Slack.

## Configuration

| Setting | Type | Status | Purpose |
|---------|------|--------|---------|
| `auto-update tracker` | label | created | Applied to the dashboard tracking issue. |
| `SECURITY_SCAN_PAT` | secret | reused | Same PAT `security-scan.yml` uses; needs `contents: write` on the security repo. Archive mirror is skipped if absent. |
| `SLACK_WEBHOOK_URL` | secret | **you add** | Incoming-webhook URL for drift notifications. Unset ⇒ Slack step is a no-op (everything else still runs). |

### Configuring Slack

1. In Slack: **Apps → Incoming Webhooks → Add to Slack**, pick the target channel,
   and copy the generated webhook URL (`https://hooks.slack.com/services/…`).
   (Or create a Slack app → *Incoming Webhooks* → *Add New Webhook to Workspace*.)
2. In GitHub: **Settings → Secrets and variables → Actions → New repository secret**,
   name `SLACK_WEBHOOK_URL`, paste the URL.

The workflow posts only on a `❌` (a short message + run link); it never posts on
green runs.

## Gating version bumps

A PR that touches the harness descriptors or pins (`internal/config/harness.go`,
`internal/e2e/harnesses.go`, `internal/e2e/versions.go`) triggers
[`harness-contract.yml`](../.github/workflows/harness-contract.yml), which runs the
model-free `contract` + `launch` stages against the **pinned** version before merge —
no token, no model call. Latest-release drift is caught nightly.

Run the smoke tier locally:

```sh
go test -tags=e2e -run 'TestHarnessCLIContract|TestHarnessLaunchProbe' -v ./internal/e2e/
```
