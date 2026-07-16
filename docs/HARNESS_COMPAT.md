# Harness compatibility tracking

omac supports several inner harnesses (opencode, claude-code, codex, copilot, pi).
A harness release can silently break omac by renaming a CLI flag, moving a
subcommand, or changing a config schema. The weekly
[`E2E: drift` workflow](../.github/workflows/e2e-smoke.yml) (also manually
dispatchable) is the tripwire: it installs every harness's **latest** release and
records three stages per harness, against **two omac versions** — `main` (built
from source) and `release` (the latest published binary, i.e. what users run):

| Stage | What it proves | Model call? |
|-------|----------------|-------------|
| `contract` | Every CLI flag/subcommand omac derives from its registry still exists in the harness's `--help` (see `internal/e2e/contract.go`). | no |
| `launch` | `omac start <harness>` reaches the real binary through the sandbox (PATH, config-home, sandbox admission). | no |
| `llm` | A **single lightweight** agent turn (echo-rest) — confirms the model auth/proxy path and sidecar facade. Run for every harness, claude-code included. The heavy multi-probe security-audit stays in the pinned weekly `E2E: full`. | yes |

`✅` pass · `❌` fail · `➖` not run. The matrix is
`harness × os × omac{main,release}` (codex/macOS excluded), and rows are sorted
newest-first, then grouped by omac version, OS, and harness.

## Where the record lives

The matrix is **not** committed to this repo (no bot pushes to `main`):

- **Live dashboard** — one stable tracking issue titled *"Harness compatibility matrix"*
  (label `auto-update tracker`). Its **description is rewritten in place** every run —
  never via comments — with a status header, the currently-failing rows (if any),
  and a rolling 30-day history, newest first.
- **Permanent archive** — every run appends to a private archive repo (the same
  repo the security scans use), under a `harness-compat/` subpath, giving full
  diffable history. The repo name is not printed into the public dashboard issue.
- **Slack** — every run (pass or fail) posts a status message with links to the
  dashboard issue and the run log. On a failure it includes a short **SKAINET**-
  generated summary of exactly what broke (which harness/OS/stage and the specific
  flag or error), built from the failing legs' own logs; the same summary is
  written into the dashboard issue.

## Configuration

| Setting | Type | Status | Purpose |
|---------|------|--------|---------|
| `auto-update tracker` | label | created | Applied to the dashboard tracking issue. |
| `SECURITY_SCAN_PAT` | secret | reused | Same PAT `security-scan.yml` uses; needs `contents: write` on the security repo. Archive mirror is skipped if absent. |
| `SLACK_WEBHOOK_URL` | secret | **you add** | Incoming-webhook URL for the per-run status message. Unset ⇒ Slack step is a no-op (everything else still runs). |

### Configuring Slack

1. In Slack: **Apps → Incoming Webhooks → Add to Slack**, pick the target channel,
   and copy the generated webhook URL (`https://hooks.slack.com/services/…`).
   (Or create a Slack app → *Incoming Webhooks* → *Add New Webhook to Workspace*.)
2. In GitHub: **Settings → Secrets and variables → Actions → New repository secret**,
   name `SLACK_WEBHOOK_URL`, paste the URL.

The workflow posts on **every** run — `🟢 all green` or `🔴 N failing` (with the
SKAINET summary of what broke) — plus links to the dashboard issue and the run log.

## Running the model-free checks

The `contract` and `launch` stages need no token or model call. They run weekly as
part of `E2E: drift` (never as a separate per-PR job). On every PR, `CI` →
`E2E: model-free` also runs the pure-Go derivation unit tests (`contract_test.go`),
which verify the checked flags stay wired to the registry — without installing any
harness. Run the full model-free tier locally:

```sh
go test -tags=e2e -run 'TestHarnessCLIContract|TestHarnessLaunchProbe' -v ./internal/e2e/
```
