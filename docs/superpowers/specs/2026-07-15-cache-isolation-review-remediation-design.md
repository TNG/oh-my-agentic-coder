# Cache Isolation Review Remediation Design

## Goal

Resolve the accepted code-quality and documentation findings in the uncommitted
tool-cache isolation change while preserving its per-workdir cache model and
minimal filesystem grants.

## Scope

- Make cache cleanup lock acquisition descriptor-relative after a cache root is
  trusted, so cleanup cannot mutate a replacement directory during a root-swap
  race.
- Make `omac doctor` distinguish a whole Cargo-home read grant from the narrow
  `~/.cargo/bin` runtime grant, and consistently report host Cargo config or
  credential files that isolated `CARGO_HOME` will not use.
- Make cache E2E probes unambiguous by granting discovered tool installations
  read-only, checking Linux Bubblewrap availability before local runs, fixing
  the pip archive encoding, and verifying ephemeral-cache removal.
- Correct user-facing docs, OpenSpec language, and test comments to describe
  the implemented cache and environment contract precisely.

## Non-goals

- Do not broaden production sandbox filesystem permissions.
- Do not redirect arbitrary hardcoded third-party cache paths.
- Do not change external nono profile enforcement; document its environment
  allowlist requirements instead.
- Do not add a general runtime-discovery abstraction outside the cache E2E
  suite.

## Design

### Cleanup locking

`ClearWorkdir` already opens and validates the cache root through `os.Root`.
Add a cleanup-specific lock helper that creates and validates `.locks` and its
digest lock file relative to that trusted descriptor. It must preserve current
lock safety checks (digest validation, no symlinks, regular-file validation,
single-link validation, exclusive non-blocking flock). Normal launch locking
continues to use its existing pathname-based helper.

Regression coverage will replace the cache root between validation and lock
acquisition, then verify cleanup reports a changed root without creating,
chmodding, or otherwise modifying the replacement.

### Doctor diagnostics

`doctor` will emit a read warning when a grant structurally covers
`~/.cargo`, but not when it covers only `~/.cargo/bin`. Cargo sentinel checks
will use `Lstat` for the four known host files whenever the inspected launch
uses isolated `CARGO_HOME`; the warning remains content-free and advisory.

### E2E cache probes

The cache E2E suite will resolve each host-installed tool's required runtime
roots before writing the sandbox profile, then add only those roots as
read-only grants. The cache scope remains the only writable cache location.
Each probe will report the resolved executable/runtime location so a failed
test identifies visibility problems separately from cache behavior.

On Linux, the suite will use the existing Bubblewrap availability check used
by sandbox integration tests and skip only when the local sandbox backend is
unavailable. CI retains its explicit Bubblewrap verification and therefore
fails if the image is incomplete. The pip fixture will serve a gzip-compressed
source archive for its `.tar.gz` URL. The ephemeral test will retain the
reported cache path and assert it is removed after the child exits.

### Documentation contract

Docs will state that only supported tool and XDG-aware cache variables are
redirected, and that runtime toolchain paths such as `~/.rustup` may be
read-only visible. `--no-sandbox` will be described as retaining normal OMAC
and temporary-directory injection while omitting cache-scope creation and
cache redirects. Cargo registry tokens will be documented as inherited host
variables explicitly allowed by the sandbox profile, not sidecar
`env_passthrough`. External nono profiles that use `environment.allow_vars`
will list the eight cache-related variables required to preserve redirects.

## Verification

- Unit tests cover Cargo-home warning boundaries, Cargo sentinel reporting,
  descriptor-relative cleanup races, and cache environment handling.
- Cache E2E tests prove tool execution under narrow read grants, isolated cache
  writes, real pip archive installation, ephemeral cleanup, and Linux backend
  handling.
- Relevant Go package tests and the deterministic Docker cache suite pass.
- Documentation claims are compared against `toolcache.Environment`, launch
  environment injection, and profile filtering behavior.
