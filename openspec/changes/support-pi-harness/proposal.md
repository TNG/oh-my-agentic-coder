## Summary

Add Pi (`pi`) as a supported harness in omac, following the same declarative
pattern used for OpenCode, Claude Code, Codex, and Copilot.

## Motivation

omac's harness registry is built for extensibility: adding a new agentic
coder is a matter of registering one descriptor (name + aliases, inner
command, server-launch convention, bridge), not editing call sites. Today
the registry ships four harnesses — `opencode`, `claude-code`, `codex`,
`copilot`. Teams that standardize on Pi (pi.dev) cannot use the sandbox, the
skill marketplace, or any skills. The Go core is already harness-agnostic
(it execs whatever `inner_cmd` resolves to and exposes a stable `OMAC_*` +
`/__omac__/` contract), so the remaining work is to register the new
harness and supply a per-harness bridge — bringing the total to five
supported agentic coders.

## What Changes

- Register the `pi` harness descriptor in `harnessRegistry()` (inner command
  `pi`; session-list kind; bridge = `.pi/extensions/`). Pi has a TypeScript
  extension system with `session_start` and `before_agent_start` lifecycle
  events — the bridge is a TS extension (like the OpenCode plugin) that POSTs
  to the control plane on session lifecycle, injects the skills manifest via
  `before_agent_start`, and reads `OMAC_SANDBOX_BRIEFING` for system-prompt
  injection.
- Add a new `SessionListKind` enum value (`SessionListPi`) and the
  corresponding `listPi()` session-listing function. Pi sessions are JSONL
  files under `~/.pi/agent/sessions/`, organized by working directory.
- Ship a native bridge: `.pi/extensions/omac-bridge/index.ts` — a TypeScript
  extension that POSTs to `OMAC_CONTROL_BASE` `/__omac__/activate` and
  `/__omac__/deactivate`, injects the skills manifest into session context,
  and exposes per-skill base URLs under `OMAC_<MOUNT>_BASE` /
  `OMAC_G_<MOUNT>_BASE`, inert when `OMAC_CONTROL_BASE` is unset.

## Capabilities

### New Capabilities
- `pi-harness`: A registered `pi` harness descriptor + bridge wiring Pi
  CLI into the omac control plane and `OMAC_*` env contract — TypeScript
  extension with `session_start`/`before_agent_start` lifecycle,
  per-directory skill activation, system-prompt skills manifest injection
  (via `before_agent_start`), and per-session skill base-URL injection.

### Modified Capabilities
- `inner-harness`: The harness registry gains one new descriptor (`pi`)
  and one new `SessionListKind` value. The registry pattern and
  positional-subcommand UX are unchanged.
- `agent-bridge`: The bridge interface gains one concrete bridge (Pi).
  Pi uses a TypeScript extension (same language as OpenCode); the bridge
  interface contract is unchanged.

## Impact

- **Code (Go):** `internal/config/harness.go` (one new descriptor in
  `harnessRegistry()`, one new `SessionListKind` enum value),
  `internal/session/session.go` (`listPi()` + dispatch case + `piRoot`
  parameter added to `list()` signature). Harness-agnostic core
  (`facade`, `supervisor`, `sandbox`, `registry`, `skillsource`, control
  plane) reused unchanged.
- **Bridges:** existing bridges retained unchanged in behavior; new Pi
  bridge assets under `.pi/`.
- **Env/contract:** `OMAC_*` naming reused as-is; the bridge reads the
  same vars.
- **Skills & marketplace:** no per-skill code changes; skills already
  speak the `OMAC_*`/REST contract.
- **E2e tests:** new harness config in `internal/e2e/harnesses.go`
  (`piConfig()`) + pinned version in `versions.go`.
- **Docs:** README.md harness list, CREATING_A_SKILL.md skills-dirs
  section, docs/MULTI_DIR_DESKTOP.md bridge table.
- **Backward compatibility:** omitting the harness still defaults to
  `opencode`; existing layouts and bridges keep working unchanged.

## Scope

In scope:
- Harness descriptor + session listing for `pi`
- Bridge assets (TypeScript extension) for pi
- E2e test config + pinned version
- Documentation updates (README, CREATING_A_SKILL, MULTI_DIR_DESKTOP)
- Unit tests

Deferred to separate change:
- (none remaining — all items implemented in this change)
