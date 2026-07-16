# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`omac` (oh-my-agentic-coder) is a single-binary Go CLI that bridges out-of-sandbox
REST/HTTP "sidecar" services into a sandboxed agentic-coding environment through one
Unix-domain-socket facade. Per-skill **secrets live in the OS keychain** and are
injected into sidecar processes at spawn time — they never reach the sandbox or disk.

The authoritative design spec is [`oh-my-agentic-coder.md`](./oh-my-agentic-coder.md)
(section numbers are referenced throughout the code, e.g. "§10.6"). When code and the
README disagree, the design doc usually wins. `CREATING_A_SKILL.md` is the full skill
authoring/`omac.yaml` reference.

## Commands

```bash
go build -o omac ./cmd/omac            # dev build (Version reports "0.1.0-dev")
go test ./...                          # all unit + integration tests
go test -race -count=1 ./...           # what CI runs
go test ./internal/facade/ -run TestFacadeEchoLikeRest   # single test
gofmt -l .                             # must print nothing (CI fails otherwise)
go vet ./...
staticcheck ./...                      # CI lint (dominikh/staticcheck)
```

CI (`.github/workflows/ci.yml`) runs build + `-race` tests on **both Linux and macOS**,
plus `gofmt`/`staticcheck`. Platform-specific code (keychain, sandbox launchers) is split
by build tags / `_linux.go` / `_darwin.go` suffixes — when touching those, consider both
platforms.

Many facade/serve tests open a loopback TCP port or Unix socket and **skip automatically**
when `connect(2)` is denied (e.g. inside a hardened sandbox). A skipped test here is not a
failure. End-to-end smoke test: `bash scripts/serve_smoke.sh` (needs `curl`, `python3`,
loopback; expects "ALL GREEN").

Release builds use GoReleaser (`.goreleaser.yaml`); see `docs/DEVELOP.md` for the
release-style ldflags and snapshot build.

## Architecture

### Runtime data flow (`omac start`)

```
register (→ keychain + sidecar.json + skill-config.yaml)
                     │
   start ──► supervisor: spawn sidecars on ephemeral 127.0.0.1 ports,
             inject secrets+config via env only, health-probe each
                     │
             facade: bind a Unix socket, reverse-proxy /<mount>/<rest>
             → http://127.0.0.1:<port>/<rest> (streams SSE + WS, no buffering)
                     │
             sandbox: launch the inner harness with only the socket +
             granted paths visible; secrets/config files are NOT mounted
                     │
             inner harness (opencode | claude) reaches sidecars via $OMAC_SOCKET
```

The sandbox sees resolved **values** (env vars, the bridge socket path), never the config
files that produced them.

### Package map (`internal/`)

- `cli/` — subcommand dispatch. `cli.go` is the entry/registry; one file per subcommand
  (`start.go`, `register.go`, `serve.go`, `doctor.go`, …). Exit codes are constants in
  `cli.go` (mirror design §10.6 / README "Exit codes").
- `config/` — `oh-my-agentic-coder.yaml` (launcher) + `omac.yaml` (skill meta) types, and
  `harness.go` (see below).
- `supervisor/` — sidecar lifecycle: spawn, env construction, health probe, shutdown.
- `facade/` — the Unix-socket HTTP reverse proxy; streaming + WebSocket upgrade splicing;
  serve-mode route states (live / stub-409 / broken-502).
- `sandbox/` + `sandboxprofile/` + `sandboxrun/` — the **built-in sandbox**.
  `sandboxprofile` parses/expands JSON profiles; `sandboxrun` is the kernel backend
  (Seatbelt SBPL on macOS via `*_darwin.go`, bubblewrap + Landlock on Linux via
  `*_linux.go`, re-exec'd as `omac sandbox run`/`stage2`).
- `netproxy/` + `netprompt/` — the filtering CONNECT proxy and the interactive
  allow/deny network dialog (zenity/kdialog/osascript) with learned-policy persistence.
- `keychain/` + `secrets/` — OS-keychain wrapper and the redacted, zeroize-able `Secret`
  type (never logged, never on argv).
- `registry/` + `skillconfig/` + `skillsource/` + `skillconfig` — `sidecar.json` registry
  (atomic writes, flock), non-secret `skill-config.yaml`, and skill discovery/search-order.
- `plugin/` + `opencodestate/` — client-side bridge install (OpenCode plugin asset under
  `internal/plugin/assets/`) and OpenCode project-state reading for `serve`.

### Two extension points are data-driven, not branchy

1. **Harnesses** (`internal/config/harness.go`): adding an inner agentic coder (opencode,
   claude-code) means appending one `Harness` descriptor + shipping its bridge — there is
   **no command-dispatch switch on harness name**. Aliases, default inner cmd, server-mode
   convention, bridge dir, and skills-base all live as fields. Skill discovery is
   **harness-scoped**: each harness scans its own `SkillsBase` (`opencode`→`.opencode/skills`,
   `claude`→`.claude/skills`) **plus** the shared `.agents/skills`, and never the other
   harness's dir.
2. **Sandbox profiles**: selected by name (`builtin`, `nono`, `no-sandbox-debug`, …) via a
   template mechanism, not hardcoded launch logic.

When adding a harness or sandbox backend, follow the existing descriptor/profile pattern
rather than introducing conditional branches at call sites.

### Bridges (client side, per harness)

The inner harness is wired to omac's control plane by a small bridge shipped in-repo:
- OpenCode: `.opencode/plugins/omac-multidir.ts` (typechecked in CI via `tsc`, see
  `docs/DEVELOP.md`).
- Claude Code: `.claude/settings.json` `SessionStart`/`SessionEnd` hooks →
  `.claude/hooks/omac-bridge.sh`.

Skills themselves are harness-agnostic — the same `SKILL.md` + `omac.yaml` work under any
harness. The reference skill is `.opencode/skills/echo-rest/` (stdlib-only Python sidecar).

## Conventions

- **Stdlib-first**: dependencies are deliberately minimal (`go-keyring`, `golang.org/x/{term,sys}`,
  `yaml.v3`). Prefer the standard library; justify any new dependency.
- **Spec-driven changes**: significant features are proposed under `openspec/changes/<name>/`
  (`proposal.md` / `design.md` / `tasks.md` / `specs/`) before/alongside implementation.
  The `.opencode/commands/opsx-*` + `openspec-*` skills drive that workflow. Existing
  changes (`native-sandbox`, `support-claude-code-harness`, `harness-scoped-skill-discovery`)
  are the model for new ones.
- Secrets must never hit argv, logs, disk, or the sandbox — use `secrets.Secret`, pass via
  env only, and keep them out of `sidecar.json` / `skill-config.yaml`.
