# Tool cache isolation


omac redirects the caches supported by `XDG_CACHE_HOME`, `GOCACHE`,
`GOMODCACHE`, `NPM_CONFIG_CACHE`, `PIP_CACHE_DIR`, and `CARGO_HOME`
into a per-scope directory it owns. The cache scope is selected by the
`cache.scope` config key (see [Modes and trust
domains](#modes-and-trust-domains)) and exposed to the inner process via
two selector variables and six tool-specific redirects:

| Variable | Points at | Purpose |
|---|---|---|
| `OMAC_CACHE_DIR` | the selected cache scope directory | selector — names this directory |
| `OMAC_CACHE_MODE` | `persistent` or `ephemeral` | selector — names the mode |
| `XDG_CACHE_HOME` | `$OMAC_CACHE_DIR/xdg` | generic XDG cache (used by opencode, gh, …) |
| `GOCACHE` | `$OMAC_CACHE_DIR/go-build` | Go build cache |
| `GOMODCACHE` | `$OMAC_CACHE_DIR/go-mod` | Go module download cache |
| `NPM_CONFIG_CACHE` | `$OMAC_CACHE_DIR/npm` | npm cache |
| `PIP_CACHE_DIR` | `$OMAC_CACHE_DIR/pip` | pip cache |
| `CARGO_HOME` | `$OMAC_CACHE_DIR/cargo` | cargo home (registry index, git checkouts, …) |

Hardcoded host cache locations (e.g. `~/.cache`, `~/Library/Caches`,
`~/.cargo`, `~/.npm`) are denied by the default `builtin` profile.
Unsupported third-party tools that hardcode another cache path need an
explicit profile configuration; omac does not redirect them
automatically. Only the selected cache scope leaf is writable for
caches. `omac sandbox run` re-derives the same map only after it has verified that
`OMAC_CACHE_DIR` matches an exact writable grant in the active sandbox
profile, so an inherited tool-specific variable cannot bypass the
profile's environment allowlist.

## Modes and trust domains

omac selects the persistent cache scope from the `cache.scope` config
key (overridable per launch with `--cache-scope`). It defaults to
`global` — one cache shared by every workdir, mirroring a normal dev
box's single `~/.cache`. Two narrower scopes trade sharing for
isolation:

| `cache.scope` | Scope identity | Shared across |
|---|---|---|
| `global` (default) | `v1:shared` | all workdirs and all configs |
| `config` | `v1:config:<canonical config path>` | all workdirs governed by the same launcher config file |
| `workdir` | `v1:workdir:<canonical workdir>` | that one workdir only |

All persistent scopes resolve to `~/.cache/omac/<sha256(identity)>`. The
launch command and flags then map onto the chosen scope:

| Launch | Persistent scope | Mode |
|---|---|---|
| `omac start` / `omac serve` (default) | `global` → `v1:shared` | `persistent` |
| `--cache-scope config` (with a config on disk) | `v1:config:<config path>` | `persistent` |
| `--cache-scope workdir` (`omac start`, `omac serve --workdir`) | `v1:workdir:<canonical workdir>` | `persistent` |
| `omac serve --cache-scope workdir` (no `--workdir`) | `v1:serve:<canonical launch workdir>` | `persistent` |
| `--ephemeral-cache` | per-launch sandbox temp dir (removed on exit) | `ephemeral` |
| `--no-sandbox` | none — no scope prepared | — |

- **`global` (default).** One cache shared by everything. Cheapest, and
  what most developers already expect; the redirected tool caches (Go,
  npm, cargo, pip) are concurrency-safe, so parallel launches sharing
  one directory is fine. Note the trust trade-off: concurrency-safety is
  **not** isolation — under `global` a build in one workdir can leave
  artifacts in the shared cache that a build in another workdir later
  reads. Choose `workdir` (or `--ephemeral-cache`) when cross-workdir
  cache poisoning is a concern.
- **`config`.** All workdirs that load the same launcher config file
  share a cache, keyed on the config file's canonical path. Falls back
  to `global` when no config file is on disk (compiled-in defaults).
  Good for a team config that governs many project directories.
- **`workdir`.** Each workdir gets its own cache, keyed on the canonical
  workdir path, **not** the bare directory name — `~/work/acme` and
  `~/clients/acme` are two different scopes, as are the **main worktree**
  and a **linked worktree** of the same repository (their absolute paths
  differ). This is the strongest isolation and was the pre-#158 default.
- **Accepted same-domain poisoning.** Two paths that resolve to the
  same canonical absolute path share a scope. omac does not try to
  distinguish them; identity follows the directory, not the label.
- **`--ephemeral-cache`.** A fresh directory under the per-launch
  sandbox temp dir, removed when the inner command exits. Use it when
  you want a clean-room build or when persistent-scope setup fails
  (e.g. an unsafe `~/.cache/omac` symlink) — omac's failure hint
  names the flag. `--ephemeral-cache` cannot be combined with
  `--no-sandbox` (there is no sandbox to grant the temp dir).
- **`--no-sandbox`.** Disables the entire omac sandbox (filesystem
  isolation, network egress filtering, secret isolation, env
  filtering). No cache scope is prepared and no `OMAC_CACHE_*` /
  `XDG_CACHE_HOME` / tool redirects are injected. Normal `OMAC_*`
  transport variables and the per-launch `TMPDIR` remain available to
  the inner command. This is a debug escape hatch, not a security
  boundary.
- **Multi-directory `omac serve`.** Because one serve process shares a
  single sandbox across every activated directory, per-workdir isolation
  only applies to `omac serve --workdir <dir>`. Under `--cache-scope
  workdir` without `--workdir`, the process falls back to a per-launch
  serve-scope cache (`v1:serve:<canonical launch workdir>`); `global`
  and `config` behave as above.

## Cleaning up cache scopes

```text
omac cache clear           # Remove the active cache scope (per cache.scope).
omac cache clear --all     # Remove every inactive cache scope (destructive).
```

`omac cache clear` removes the persistent cache scope the current config
resolves to (`global`, `config`, or `workdir`). `omac cache clear --all`
walks every scope under `~/.cache/omac` and removes the inactive ones.
Each scope is reported
as:

- `removed` — the scope was inactive and has been deleted;
- `active` — the scope's lock is currently held by a running launch,
  so it was left intact;
- `skipped` — the scope is unsafe, missing, or was replaced between
  the open and the remove (e.g. a symlink swap, a TOCTOU replacement),
  so it was not touched.

Active scopes are never removed: omac holds a shared lock on a
persistent scope for the lifetime of the launch, and
`omac cache clear --all` takes an exclusive lock per scope, so a
running launch's cache is always skipped. This is the active-scope
refusal: a scope in use cannot be deleted from under a running agent.

## Private Cargo registry setup

Because `CARGO_HOME` is redirected to `$OMAC_CACHE_DIR/cargo`, the
host's `~/.cargo/config` and `~/.cargo/credentials` are not picked up
inside the sandbox. Supply a private registry token via:

1. A project-local `.cargo/config.toml` that declares the registry
   (e.g. under the `[registries.<name>]` table).
2. Export `CARGO_REGISTRIES_<NAME>_TOKEN` in the environment that
   starts `omac`, where `<NAME>` is the registry key uppercased with
   `-` changed to `_`. If the sandbox profile sets
   `environment.allow_vars`, include that exact token variable.
   Cargo receives it through the sandboxed harness environment;
   `sidecar.env_passthrough` configures only the sidecar.

`omac doctor` detects the presence of `~/.cargo/config`,
`~/.cargo/config.toml`, `~/.cargo/credentials`, and
`~/.cargo/credentials.toml` with `Lstat` only, never reading or
copying them, and warns that an isolated `CARGO_HOME` will not use
them.

