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

## Nix flake

`flake.nix` packages `cmd/omac` from a pinned release source, not the current
checkout, for the systems listed in its `eachSystem` call. Ordinary Go dependency
updates therefore do not change the Nix package or invalidate its hashes.
On Linux, its wrapper provides the Quick Start runtime executables:
Bubblewrap, Zenity, and `notify-send` from libnotify. When the platform
prerequisites change, update both this wrapper and the NixOS instructions in
the [Quick Start](../getting-started/quick-start.md).

The `release` block in `flake.nix` holds the version, exact source commit, source
hash, and `vendorHash` together. The [Nix release workflow](#nix-release-publication)
updates those fields automatically after publication; no separate dependency
metadata file or manual hash update is needed for ordinary releases. It uses
the flake's pinned nixpkgs and Go toolchain to calculate the vendor hash.
Changing the required Go version may still require updating the toolchain in
the flake. Update `flake.lock` deliberately with `nix flake update`, not during
automatic release packaging.

Run these checks after a flake, dependency, or version update:

```bash
nix flake check --all-systems --no-build
sh scripts/flake_test.sh
python3 scripts/update-nix-release_test.py
python3 scripts/nix-release-workflow_test.py
```

The first command evaluates every supported system; the smoke script builds
and runs the package on the current system and checks its reported version.
The Python tests exercise hash-update failures and publication against temporary
local Git repositories, without making GitHub changes. These do not replace
the [Go tests](testing.md), which test the current checkout rather than the
pinned Nix release.

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

### Nix release publication

After GoReleaser publishes a `v*` tag, the reusable
[Nix release workflow](https://github.com/TNG/oh-my-agentic-coder/blob/main/.github/workflows/nix-release.yml)
does the following:

1. Checks out `main` and resolves the published release tag to its exact source commit.
2. Runs `scripts/update-nix-release.py` to update only the four release fields in
   `flake.nix`. Failed hash updates restore the original file. Separate Linux x86-64,
   Linux ARM64, and macOS ARM64 jobs build and check the same prepared flake.
3. Creates a packaging-only commit on `automation/nix-<version>`, and atomically
   pushes that branch and a permanent `nix-<version>` tag pointing to the commit.
4. Opens a PR back to `main` using the repository template. Normal review and
   branch protection still apply; the workflow never pushes to `main`.

For example, `v0.10.0` identifies the application release and `nix-0.10.0`
identifies its Nix packaging. Consumers can use the Nix tag immediately, even
before the packaging PR is merged. Deleting the automation branch after merging
must not delete the tag. The flake fetches the application source from the
`v0.10.0` commit, not from its own `nix-0.10.0` commit, avoiding a circular source
hash. Files elsewhere in the Nix-tagged checkout can reflect a later `main`.
Existing `v*` tags are never moved; releases predating this automation do not
automatically acquire Nix tags.

**Repository setup:** provide `NIX_RELEASE_TOKEN`, a dedicated fine-grained PAT
with Contents and Pull requests read/write permissions on this repository. It
must be allowed to create `automation/nix-*` branches and `nix-*` tags, but needs
no bypass for protected `main`. Publication runs on a fresh runner, and the token
is exposed only to the final publication step, not any Nix build or smoke test.
Using a dedicated token rather than `GITHUB_TOKEN` allows the PR
to trigger normal CI. Configure tag rules to prohibit updating or deleting
`nix-*` tags; the workflow itself never force-pushes or replaces them.

For a dry run or recovery, run **Nix release** from the Actions UI on `main`,
specifying the published `v*` tag. **Dry run** defaults to enabled and performs
validation without creating commits, tags, or PRs. Disable it to publish. Runs
for different release versions are independent; concurrent attempts at the same
version are serialized.
If `main` advances during validation, preparation stops instead of rebasing;
rerun against the new `main`. A newer version already packaged on `main` prevents
publishing an older version as a new update.

If the tag push succeeds but PR creation fails, rerunning reuses the permanent
tag and retries only the outstanding PR operation. An existing open or merged
PR is not duplicated. A closed PR, missing automation branch, or conflicting tag
requires manual investigation; never repair it by moving the permanent tag.
A Nix packaging failure does not retract the application release, and previously
published Nix tags remain usable.

## CI

CI (`.github/workflows/ci.yml`) gates every PR on `gofmt`, `go vet`, `staticcheck`, build, and `go test -race`. To catch `gofmt` drift before pushing, install the pre-commit hook:

```bash
scripts/install-hooks.sh
```
