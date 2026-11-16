---
title: Development
description: Building and understanding the omac codebase.
---

## Repository layout

| Directory | Purpose |
|---|---|
| `cmd/omac/` | Entrypoint |
| `internal/cli/` | Subcommand dispatch (one file per subcommand; see [CLI reference](../usage/cli.md)) |
| `internal/config/` | Types for both config files: `omac.yaml` (skill manifest) and `oh-my-agentic-coder.yaml` (launcher config) |
| `internal/registry/` | `.opencode/sidecar.json` (atomic writes, flock) |
| `internal/keychain/` | Thin wrapper over `github.com/zalando/go-keyring` |
| `internal/secrets/` | Secret type (redacted Stringer, zeroize) + masked prompt |
| `internal/osinfo/` | macOS / Linux / WSL detection |
| `internal/facade/` | Unix-socket HTTP reverse proxy (SSE + upgrades) |
| `internal/supervisor/` | Sidecar lifecycle (spawn, health, shutdown) |
| `internal/sandbox/` | Templated sandbox-backend launcher |

## Build

Dev build (version reports as `0.1.0-dev`):

```bash
go build -o omac ./cmd/omac
```

Release-style build — stripped, reproducible, version-stamped (same ldflags as GoReleaser):

```bash
go build -trimpath -ldflags "-s -w -X main.Version=0.1.0-local" -o omac ./cmd/omac
./omac version   # -> omac 0.1.0-local
```

For full multi-platform artifacts (`.deb`, `.pkg.tar.zst`, checksums), use GoReleaser (`goreleaser release --clean --snapshot --skip=publish`). See [`.goreleaser.yaml`](https://github.com/TNG/oh-my-agentic-coder/blob/main/.goreleaser.yaml).

## Documentation site

This directory is published at
[tng.github.io/oh-my-agentic-coder](https://tng.github.io/oh-my-agentic-coder/) by
the Docusaurus project in `website/`, which reads the markdown from `docs/` in
place — nothing is copied, so every page stays readable as a plain file on GitHub.

Deployment is automatic: merging a PR that touches `docs/` or `website/` pushes
to `main`, and that push builds and deploys the site. There is no manual step.
The same workflow builds (but never deploys) on every PR, so a dead link or an
MDX parse error fails your PR rather than the live site — run `npm ci && npm run
build` in `website/` to see it before pushing.

## Releases

Pushing a git tag such as `v1.2.3` starts the release workflow
([`.github/workflows/release.yml`](https://github.com/TNG/oh-my-agentic-coder/blob/main/.github/workflows/release.yml)), which
builds the binaries and publishes them.

### Tag format

Tags follow [Semantic Versioning](https://semver.org/): `vMAJOR.MINOR.PATCH`,
with two optional suffixes:

- A `-` suffix marks a **pre-release** (example: `v1.2.3-rc.1` (release candidate 1))
- A `+` suffix is **build metadata**: an informational label for one particular
  build, such as a CI run number. It is ignored when comparing versions, so
  `v1.2.3+build.42` counts as the same version as plain `v1.2.3`.

### Release process

The release is built by [GoReleaser](https://goreleaser.com/).

| Tag type | GitHub release | Homebrew tap | Slack |
| --- | --- | --- | --- |
| Stable (`v1.2.3`) | Published as a normal release | Formula updated | Announcement posted |
| Pre-release (`v1.2.3-rc.1`) | Published as a pre-release | Not updated | No announcement |

## CI

CI (`.github/workflows/ci.yml`) gates every PR on `gofmt`, `go vet`, `staticcheck`, build, and `go test -race`. To catch `gofmt` drift before pushing, install the pre-commit hook:

```bash
scripts/install-hooks.sh
```

### Automated code review

The separate **OCR review** workflow uses OpenCodeReview 1.13.10 to add English
inline findings and update one summary comment. It reviews non-draft PRs from
branches in this repository targeting `main` when they are opened, updated,
reopened, or marked ready for review. Fork PRs are skipped.

Reviews use high effort and the existing `SKAINET_INTERNAL` (gateway base URL)
and `SKAINET_TOKEN` repository secrets. The model follows the OpenCode pin in
`internal/e2e/versions.go`, verified by `scripts/probe-model.sh` before each run.
No separate GitHub token is needed. Each push can incur LLM costs; successful
complete reviews record a checkpoint so later pushes review only new changes.
If the checkpoint is no longer valid, OCR reviews the whole PR again. Existing
inline findings are filtered to reduce duplicates.

To force a full review, add the `ocr-full-review` label, then push a commit or
reopen the PR. Adding the label alone does not start a run. Remove it afterwards
to restore incremental reviews. Retrying a failed run is available under
Actions → OCR review → the run → Re-run jobs; a successful run retried at the
same commit normally has nothing new to review.

This workflow becomes active after it is merged to `main`. It uses the trusted
base checkout and fetches PR commits only as review input; it does not execute PR
scripts or install their dependencies. Runs stop after 30 minutes, and a newer
run cancels the previous run for that PR.

Reviews are advisory: **do not add OCR review to required status checks or
required workflows** in branch protection or repository rulesets. Findings do
not fail the job; technical errors remain visible as a failed OCR run, separate
from the existing CI checks.

## Reviewing pull requests with agents

The repo ships two agent skills in `.agents/skills` (linked from `.claude/skills` for Claude Code). Ask your agent to use them by name, for example "review PR 487 with the agent-review skill".

- **`agent-review`** runs the project's tests at the PR head and on the base, picks the reviewers that fit the PR (code style, error handling, tests, documentation, UX consistency, security, complexity, performance), checks each of their findings against the code, and writes one PR comment with numbered findings, each with a type and a severity. To get findings fixed, reply e.g. "Please address F2, F5" and point an agent at the comment; it commits the fixes on a local `review-fixes/<N>` branch and leaves pushing to you.
- **`explain-pr`** writes an explainer page for a complex PR: what it changes, where that fits in the codebase, the usage scenarios that matter, alternative approaches, and how to review it. The result is one HTML page: an Artifact link in Claude Code, a file you open in a browser elsewhere. It needs no extra tools; only its diagrams load from a CDN (cdn.jsdelivr.net).

Both read the PR with `gh`, the omac `omac-gh` skill, or a GitHub MCP server. `agent-review` posts a comment only when you ask it to.
