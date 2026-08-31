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

For full multi-platform artifacts (`.deb`, `.pkg.tar.zst`, checksums), use GoReleaser (`goreleaser release --clean --snapshot --skip=publish`). See [`.goreleaser.yaml`](../../.goreleaser.yaml).

## Releases

Pushing a git tag such as `v1.2.3` starts the release workflow
([`.github/workflows/release.yml`](../../.github/workflows/release.yml)), which
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
