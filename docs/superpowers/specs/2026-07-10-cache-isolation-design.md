# Sandbox Tool Cache Isolation Design

**Issue:** [#26: Sandbox: isolate tool cache from ~/.cache](https://github.com/TNG/oh-my-agentic-coder/issues/26)

**Status:** Design confirmed in the issue discussion. This document records the
agreed threat model, compatibility findings, and implementation contract before
an implementation plan is written.

## Problem

The built-in sandbox profile grants the complete host `~/.cache` and
`~/Library/Caches` trees read-write. Every sandboxed harness can therefore read
cache data belonging to unrelated host applications and modify files later
consumed outside omac. The same profile also grants broad writable tool homes
such as `~/go`, `~/.cargo`, and `~/.rustup`.

The grant is both a confidentiality risk and a cache-poisoning persistence
channel. Removing it without redirecting common tools would, however, break
normal builds or force every user to maintain custom sandbox grants.

## Goals

- Remove broad host-cache access from new built-in profiles.
- Preserve warm caches by default without sharing them across independent
  sandbox trust domains.
- Make Go, npm, pip, Cargo, XDG-aware tools, and supported harnesses work with
  isolated writable cache locations.
- Provide an opt-in mode for users who do not want cache state to survive a
  session.
- Warn users whose existing profile retains broad cache access, without
  silently modifying their configuration.
- Preserve access to installed developer tools while separating executable
  locations from writable cache locations.
- Provide explicit recovery and cleanup commands without silently weakening
  the sandbox boundary.

## Non-Goals

- Preventing persistent-cache poisoning between sessions in the same default
  trust domain.
- Automatically migrating or rewriting existing user profiles.
- Redirecting every application-specific hardcoded cache path.
- Isolating harness configuration, authentication, or session stores. Existing
  harness-specific `SandboxDirs` remain intentional grants except for the
  cache entries explicitly removed by this design.
- Redesigning tool installation or dynamically resolving every command an
  agent may execute after harness startup.
- Automatic garbage collection, age-based retention, or cache size limits.
- Isolating projects from each other inside one multi-directory `serve`
  process; those projects already share one sandbox and filesystem boundary.

## Threat Model

The normal mode protects host applications and independent omac trust domains
from reading or poisoning one another's caches. It intentionally uses a
persistent writable cache within a trust domain so subsequent sessions stay
warm.

An untrusted session can poison its own persistent cache for a later session in
the same trust domain. This is an accepted performance trade-off, not an
oversight. Users who need protection from that scenario opt into
`--ephemeral-cache`.

`--no-sandbox` remains outside this security model. Cache isolation is a
sandbox feature and is not claimed as a boundary when the sandbox is disabled.

## Trust Domains

Cache identity follows the actual sandbox boundary rather than assuming every
command maps to exactly one project:

- `start`, `continue`, and `resume` use the canonical workdir as their trust
  domain.
- The same canonical-path rule applies to both a repository's main worktree and
  its linked worktrees. No Git-specific cache identity or separate worktree
  implementation is introduced. Because each worktree has a different
  canonical working-tree path, the main worktree and every linked worktree get
  distinct writable caches even though they share one Git common directory.
  The shared Git directory, branch name, and `.git` representation (directory
  for a main worktree, indirection file for a linked worktree) are not part of
  the cache identity.
- Single-directory `serve --workdir <dir>` uses that canonical directory.
- Multi-directory/Desktop `serve` uses one persistent serve scope based on the
  canonical launch workdir and a distinct `serve` domain prefix. All projects
  activated in that process share the cache because they already share one
  sandbox process and its granted worktrees.
- `--ephemeral-cache` always uses a fresh per-process domain and does not reuse
  persistent state.

The trust-domain distinction must be documented. In particular, the project
must not claim strict per-workdir cache isolation for multi-directory `serve`.

## Cache Layout

Persistent caches live below:

```text
~/.cache/omac/<sha256(scope-identity)>/
```

The scope identity contains a versioned domain prefix and the canonical path,
for example `v1:workdir:/canonical/project` or
`v1:serve:/canonical/launch-dir`. The full SHA-256 hex digest is used; a short
six-character digest is not an adequate isolation identifier.

Canonicalization performs absolute-path cleaning and symlink evaluation before
hashing. Failure to canonicalize or create the persistent cache is a launch
error; omac must not fall back to a broad host cache. The error identifies the
failed path and tells the user to retry with `--ephemeral-cache`, which bypasses
persistent identity calculation and persistent directory creation entirely.

Consequently, two path aliases that resolve to the same worktree reuse one
cache, while two linked worktrees of the same repository receive different
caches. Moving a worktree changes its identity and produces a cold cache;
deleting and later recreating a worktree at the same canonical path reuses that
path's existing cache.

omac creates the `~/.cache/omac` root and selected scope directory before
launch. The omac root and scope leaf are mode `0700`. An existing scope path
that is a symlink or not a directory is rejected rather than followed. The
sandbox receives read-write access only to the selected scope directory.

Ephemeral cache data lives under the already isolated per-launch sandbox temp
directory and is removed with that directory on normal exit. A process killed
without cleanup may leave the operating system's temporary directory behind;
it is never reused as a persistent cache.

## Environment Redirects

The inner harness receives these injected values, overriding inherited values:

| Variable | Isolated location |
|---|---|
| `XDG_CACHE_HOME` | `<cache>/xdg` |
| `GOCACHE` | `<cache>/go-build` |
| `GOMODCACHE` | `<cache>/go-mod` |
| `NPM_CONFIG_CACHE` | `<cache>/npm` |
| `PIP_CACHE_DIR` | `<cache>/pip` |
| `CARGO_HOME` | `<cache>/cargo` |
| `OMAC_CACHE_DIR` | `<cache>` |
| `OMAC_CACHE_MODE` | `persistent` or `ephemeral` |

The hybrid redirect is intentional. `XDG_CACHE_HOME` covers OpenCode and other
XDG-aware software. The tool-specific variables are still required because
local macOS probes showed that Go, npm, pip, and Cargo did not change their
cache locations when only `XDG_CACHE_HOME` was set.

XDG-aware tools receive a cold, isolated cache on their first launch in a trust
domain. They do not inherit data from the host's previous XDG cache; that is the
intended boundary rather than a compatibility fallback.

Injected values use the existing environment overlay path, so they win over
host values and survive a profile `environment.allow_vars` filter in the same
way as other omac-injected variables.

Applications that hardcode another location, such as `~/.cache/gh`, remain
unable to access it unless the user adds an explicit profile grant. There is no
fallback to the global cache.

This behavior must be observable rather than surprising. An always-on dynamic
cache paragraph is appended to the sandbox briefing, even when the user has
overridden the general briefing text. It explains that hardcoded host caches
are denied and points the agent to `OMAC_CACHE_DIR` and the tool-specific
variables. `omac provenance` includes a cache section showing the current
workdir's default persistent scope, cache path, mode, and environment mappings.
An actual launch's authoritative values remain available through
`OMAC_CACHE_DIR`, `OMAC_CACHE_MODE`, and verbose launch output.

## Tool And Harness Access

`GOCACHE` alone is insufficient: Go stores downloaded modules separately, so
`GOMODCACHE` is part of the required redirect.

Cargo support remains in scope. `CARGO_HOME` is redirected, while
`~/.cargo/bin` and the existing `~/.rustup` toolchain remain readable but not
writable so Cargo's shim and the installed Rustup toolchain can execute. The
rest of `~/.cargo`, including credential files, is not granted. A local probe
confirmed that an isolated `CARGO_HOME` with the existing `RUSTUP_HOME` builds
a minimal project, while isolating both homes leaves Rustup without a
configured toolchain.

Because Cargo also reads global registry configuration and credentials from
`CARGO_HOME`, redirecting it intentionally prevents automatic use of the host's
Cargo credentials. Public crates.io builds need no additional cache or Cargo
configuration. Projects that use private registries provide project-local
`.cargo/config.toml` and an explicitly allowed registry-token environment
variable. omac does not copy secrets from the host Cargo home into the isolated
cache.

`omac doctor` detects the presence, but never reads or prints the contents, of
`~/.cargo/config`, `~/.cargo/config.toml`, `~/.cargo/credentials`, and
`~/.cargo/credentials.toml`, which the isolated `CARGO_HOME` will not use. It
emits an actionable warning with the project-local configuration and token
guidance so a private-registry failure is not mistaken for a network or Cargo
installation problem.

Tool-install paths are not caches. Existing read-only access to `~/.nvm` and
`~/.bun/bin` remains. `~/.cargo/bin`, `~/.rustup`, and `~/go/bin` are retained
or added as read-only executable/toolchain paths while their broad writable
parent grants are removed.

This is necessary because shebang detection and `resolveInnerBinaryPath` only
resolve the initial harness command. They do not dynamically grant later
agent-invoked commands such as `node`, `npm`, `bun`, or `cargo`.

Harness-global cache entries are removed from `SandboxDirs`:

- `~/.cache/opencode`
- `~/.cache/claude`
- `~/.cache/codex`
- `~/.cache/copilot`

Supported harness startup and lifecycle tests must pass without those grants.
If a harness requires another writable cache, implementation must use a
documented redirect into the selected isolated cache. It must not restore a
global harness cache grant.

## Default Profile Changes

The compiled-in default profile removes these read-write grants:

- `~/.cache`
- `~/Library/Caches`
- `~/go`
- `~/.cargo`
- `~/.rustup`

Read-only tool access remains narrowly scoped as described above. The existing
read-only `~/.nvm` and `~/.bun/bin` grants are not removed by this cache-focused
change.

## Existing Profiles And Doctor

The scaffolded `default.json` is authoritative after creation. omac does not
rewrite it, even when it still contains old defaults.

`omac doctor` inspects the selected built-in sandbox profile and emits an
advisory warning when a read, write, or read-write profile entry broadly covers
`~/.cache` or `~/Library/Caches`; when a write or read-write entry broadly
covers `~/go`, `~/.cargo`, or `~/.rustup`; or when a read entry exposes all of
`~/.cargo` rather than only `~/.cargo/bin`. The intended read-only
`~/.rustup` toolchain grant is not warned about. The warning names the profile
and unsafe entry, explains which isolation guarantee it bypasses, and tells the
user to remove or narrow it. It does not change the profile and does not turn
an otherwise successful doctor run into a failure.

Profiles launched through opaque external sandbox commands cannot be reliably
inspected and are outside this warning's guarantee.

Doctor also reports the Cargo configuration warning described above when host
Cargo configuration or credentials exist but will be excluded from the
isolated `CARGO_HOME`.

## CLI Behavior

`--ephemeral-cache` is available on `start`, `continue`, `resume`, and `serve`.
Normal launches use persistent cache state; the flag switches only that launch
to the per-session temporary cache.

The flag is incompatible with `--no-sandbox` and returns a usage error rather
than implying protection without a sandbox. In `serve --no-inner` mode no inner
tool cache is needed, so cache setup is skipped.

An ordinary `--no-sandbox` launch also skips cache setup and preserves the
existing host environment. It receives none of the isolation guarantees in
this document.

Verbose launch output reports whether the cache is persistent or ephemeral and
prints its path. Normal output remains quiet.

## Manual Cleanup

`omac cache clear` deletes the persistent workdir cache selected by the current
top-level `--workdir`. It uses the same canonical identity function as launch,
so it handles main and linked worktrees without separate logic.

`omac cache clear --all` deletes all persistent workdir and serve caches below
`~/.cache/omac`. The explicit `--all` is the destructive confirmation; the
command prints which caches were removed and which were skipped. Ephemeral
caches are not managed by this command.

Each persistent scope has a lock held in shared mode for the lifetime of every
launch using it. Lock files live under a sibling `~/.cache/omac/.locks/`
directory that is not granted to the sandbox, so a sandboxed process cannot
replace its lock file. Cleanup takes the lock in exclusive, non-blocking mode.
It refuses to clear the current scope while active and skips active scopes
during `--all`, reporting them to the user. Cleanup removes only validated
full-digest scope directories and never follows symlinks outside the omac cache
root.

No automatic cleanup policy is introduced. Users decide when to clear one
workdir or all accumulated caches.

## Failure Handling

- Canonicalization, directory creation, permission setup, or unsafe existing
  path errors abort before the harness starts and recommend
  `--ephemeral-cache` as the safe recovery path.
- omac never falls back to host-global caches after an isolation setup error.
- Cleanup errors for an ephemeral cache are best-effort after process exit and
  may be reported in verbose mode; they do not replace the harness exit code.
- Unsupported hardcoded application caches fail closed under the sandbox's
  normal filesystem denial.

## Rejected Interim Mitigation

Adding common sensitive cache subdirectories to the built-in protected-path
list was considered as a quick fix but rejected for this change. It would be
temporary, require overlapping migration and test work, leave unknown cache
content exposed, and can interact poorly with explicit writable grants on
macOS. The implementation should proceed directly to isolated cache domains.

## Testing Strategy

Unit and CLI tests cover:

- Canonical paths producing stable identities, including symlink aliases.
- A repository's main worktree and linked worktrees all working through the
  same path-based implementation, producing distinct cache identities, and
  being unable to access each other's cache contents.
- Distinct workdir and serve domain identities.
- Full-length digest layout, `0700` permissions, and rejection of symlink or
  non-directory scope paths.
- Exact environment redirects and precedence over inherited values.
- `OMAC_CACHE_DIR`, `OMAC_CACHE_MODE`, briefing text, and provenance cache
  output accurately describing the active/default cache boundary.
- Persistent versus ephemeral launch argument handling across `start`,
  `continue`, `resume`, and `serve`.
- Removal of broad default grants and harness-global cache entries.
- Read-only retention of required tool-install paths.
- Doctor warnings for old broad cache/tool-home profiles and no warning for
  isolated profiles.
- Doctor warnings for excluded host Cargo configuration without exposing file
  contents.
- Current-workdir and `--all` cleanup, including symlink safety and refusal or
  skipping when a scope is active.

Cross-platform integration tests on Linux and macOS cover:

- Host-global cache markers cannot be read or modified.
- The selected isolated cache is writable.
- Two workdir domains cannot access each other's caches.
- A persistent domain is reused across launches.
- An ephemeral cache starts empty and is removed after normal exit.
- Go uses isolated build and module caches.
- npm and pip report and use isolated cache paths.
- A minimal Cargo project builds with isolated `CARGO_HOME` and read-only
  Rustup/toolchain access.
- A denied hardcoded host-cache access is diagnosable from the briefing,
  injected cache variables, provenance, and documentation.
- Every supported harness still starts and completes its existing lifecycle
  test without a global harness cache grant.
- Single-directory and multi-directory `serve` use their documented scope.

Existing e2e fixtures, provenance expectations, and documentation that still
describe broad cache grants must be updated with the implementation.

## Documentation And Merge Request

User-facing sandbox documentation must explain:

- Persistent isolated caches are the default for performance.
- The normal boundary is a workdir or, for multi-directory `serve`, the shared
  serve trust domain.
- Same-domain poisoning remains possible across persistent sessions.
- `--ephemeral-cache` trades warm builds for a stronger session boundary.
- Existing profiles may require manual remediation reported by `omac doctor`.
- Hardcoded unsupported caches need explicit profile configuration.
- `omac cache clear` and `omac cache clear --all` provide manual cleanup, while
  active scopes are protected from deletion.
- Private Cargo registries require project-local configuration and an
  explicitly allowed token; host Cargo credentials are never copied.

The eventual merge request description must explicitly state the same threat
model and trade-off. It must not claim that persistent caches prevent
same-domain poisoning or that multi-directory `serve` provides per-project
cache isolation.

## Acceptance Criteria

1. New built-in profiles expose no broad host cache or writable tool-home
   grants.
2. Normal sandbox launches receive only their persistent trust-domain cache;
   ephemeral launches receive only a temporary cache.
3. Supported cache variables point inside that cache and override host values.
4. Existing profiles are never silently rewritten and broad grants produce an
   actionable doctor warning.
5. Persistent-cache setup failures offer a working `--ephemeral-cache`
   recovery path, and users can safely clear current or all inactive caches.
6. Cache behavior and unsupported hardcoded paths are visible through injected
   variables, briefing, provenance, diagnostics, and documentation.
7. Go, npm, pip, Cargo, and supported harness lifecycle tests pass on their
   supported platforms.
8. Host-cache denial, domain separation, persistence, and ephemeral cleanup are
   verified on Linux and macOS.
9. Documentation and the merge request accurately state the security boundary
   and accepted limitations.
