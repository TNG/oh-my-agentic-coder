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

## CI

CI (`.github/workflows/ci.yml`) gates every PR on `gofmt`, `go vet`, `staticcheck`, build, and `go test -race`. To catch `gofmt` drift before pushing, install the pre-commit hook:

```bash
scripts/install-hooks.sh
```
