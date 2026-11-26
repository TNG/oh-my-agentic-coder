# cli

## Overview

`cli` is the omac command-line front end (about 12k lines in 39 files). `Run`
parses the top-level `--workdir` flag, resolves the workdir to an absolute,
symlink-free path, and dispatches to a fixed set of subcommands.

Most of the code is the launch pipeline. `start`, `continue` and `resume` all go
through `runLaunch`: load config, reconcile the skill registry, validate skills
and secrets, create the runtime dir, set up the audit log, spawn skill sidecars,
build the facade, open listeners, then exec the sandboxed harness. The
long-running `serve` command serves several workdirs at once and has its own
HTTP control plane.

Example: `omac start claude --sandbox default` launches Claude Code in the
`default` sandbox policy with all registered skills mounted.

## Interface

- `Run(args []string, version string) int`: process entry point; returns the exit code.
- `Command{Name, Short, Run}` and `Env{Version, Workdir, Stdout, Stderr, Stdin}`:
  one subcommand and the state passed to it.
- Exit codes (stable contract for scripts and CI): `ExitOK=0`, `ExitGeneric=1`,
  `ExitMisuse=2`, `ExitConfigInvalid=3`, `ExitPrerequisiteMissing=4`,
  `ExitIOError=5`, `ExitSidecarHealthcheckFail=6`, `ExitSandboxAbnormal=7`,
  `ExitKeychainError=8`, `ExitSecretRefused=9`, `ExitChecksumMismatch=10`.
- Subcommands (`commands()` in `cli.go`): `register`, `deregister` (incl. `prune`),
  `list`, `secrets` (`list|set|unset|import`), `config` (`show|get`), `provenance`,
  `start`, `continue`, `resume`, `serve`, `setup`, `plugin install`, `sandbox`
  (`run`, and hidden `stage2` that `bwrap` execs on Linux), `doctor`, `diagnose`,
  `update`, `manifest`, `cache clear`, `version`.
- Key unexported pieces:
  - `runLaunch` / `launchOpts` (`start.go`): the one launch pipeline.
  - `parseLaunchArgs`: start-family flags such as `--sandbox`, `--inner`,
    `--no-sandbox`, `--ephemeral-cache`, `--accept-skill-changes`, `--audit-log`,
    `--session`, `--open-port`.
  - `runServe` / `serveServer` (`serve.go`): multi-workdir server. Endpoints
    `/__omac__/{activate,deactivate,reload,reload-global}` need a control token;
    `/__omac__/{dirs,global}` do not.
  - `startReloader` (`start_reload.go`): live reload for single-workdir `start`.
  - `controlInfo`, `notifyReload` (`controlinfo.go`): lets `register`, `secrets`
    and `config` trigger a reload in a running `serve`.
  - `sandboxPlan` / `resolveSandboxPlan`: keeps the launcher profile (`builtin`,
    `nono`, `no-sandbox-debug`) apart from the policy profile (e.g. `default`).
  - `wireFacadeSandbox`: attaches `/sandbox/denied` and `/sandbox/intent` to the facade.
  - `skillSpawnAuthorizer` (`skill_approval.go`): a sidecar may only run from the
    host-only approval snapshot.
  - `startProtectedWatch`: alerts on protected files (e.g. `.env`) created mid-session.
  - `styler` (`style.go`): ANSI output; honors `NO_COLOR`, `TERM=dumb` and non-TTY.

## Invariants

- Exit codes are a documented, stable contract. Do not renumber them.
- `commands()` and `printUsage` are maintained separately and must stay in sync.
- `start`, `continue` and `resume` must all use `runLaunch`. Do not fork the pipeline.
- Skill sidecars run outside the sandbox. Every spawn must go through
  `skillSpawnAuthorizer`, which checks the spawn path is the approval snapshot
  (not a re-hash), to avoid a check-then-exec race.
- Grandfathering global skills into the approval store must run before
  auto-register, so skills the agent wrote in the same session are not approved.
- Never pass a launcher profile name to the policy resolver; use `sandboxPlan` (#173).
- Startup order: auto-deregister vanished skills, refuse unregistered skills,
  check bundle hashes, check required config. Only bundle drift can be overridden
  (`--accept-skill-changes`).
- Auto-deregister only drops workdir-layer registry entries; secrets and config stay.
- Workdir registry and config override global, per skill and per field.
- `start_reload` only adds skills; it never drops a healthy mounted route.
- `--no-audit` together with `--audit-strict` is a misuse error.
- The cache-guidance paragraph is always appended to the sandbox briefing.
- Registry, secret and config changes go through `withRegistryLock`.
- `newRuntimeDir` never reuses an existing directory.

## Side Effects

- Writes `~/.config/omac/serve-control.json` (dir 0700, file 0600) with the
  control URL and token; removed on exit. Readers check the pid is alive and
  owned by the current user.
- POSTs to loopback `/__omac__/reload*` with the `X-Omac-Control-Token` header.
- Creates per-session runtime dirs (`$XDG_RUNTIME_DIR/omac` or
  `~/.local/state/omac/run` on Linux, `$TMPDIR/omac` on macOS); opens a Unix
  socket and an ephemeral `127.0.0.1` TCP listener.
- Spawns sidecars and the sandboxed harness; stops the process tree via `/proc` or `ps`.
- Exports `OMAC_*` env vars to the child, e.g. `OMAC_SOCKET`, `OMAC_BASE`,
  `OMAC_SKILLS`, `OMAC_HARNESS`, `OMAC_CACHE_DIR`, `OMAC_FACADE_TOKEN`,
  `OMAC_SANDBOX_BRIEFING`, `OMAC_WORKDIR`.
- Reads and writes keychain secrets, the registry (`.opencode/sidecar.json`,
  `~/.config/omac/sidecar.json`), skill config and the skill approval store.
- Writes a briefing file into the workdir and adds it to git's exclude list.
- Appends to the audit log. `update` contacts GitHub and replaces the binary.
  `setup` and `plugin` copy files into harness dirs.
- Shows dialogs/notifications when protected files appear; may run `lsof`.

## Dependencies

Uses almost every sibling package, including [`../sandboxrun`](../sandboxrun/EGM.md),
[`../facade`](../facade/EGM.md), [`../supervisor`](../supervisor/EGM.md),
[`../config`](../config/EGM.md), [`../registry`](../registry/EGM.md),
[`../skillstate`](../skillstate/EGM.md), [`../skilltrust`](../skilltrust/EGM.md),
[`../keychain`](../keychain/EGM.md), [`../audit`](../audit/EGM.md) and
[`../updater`](../updater/EGM.md). See [`../EGM.md`](../EGM.md) for the full list.

OS-specific files: `controlinfo_pid_darwin.go`, `controlinfo_pid_linux.go`.

## Verification

- Run: `go test ./internal/cli/...`
