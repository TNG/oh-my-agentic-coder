## Summary

Add Codex CLI (`codex`) and GitHub Copilot CLI (`copilot`) as supported harnesses in omac, following the same declarative pattern used for OpenCode and Claude Code.

## Motivation

omac's harness registry is built for extensibility: adding a new agentic
coder is a matter of registering one descriptor (name + aliases, inner
command, server-launch convention, bridge), not editing call sites. Today
the registry ships two harnesses — `opencode` and `claude-code`. Teams that
standardize on Codex CLI (`codex`) or GitHub Copilot CLI (`copilot`) cannot
use the sandbox, the skill marketplace, or any skills. The Go core is
already harness-agnostic (it execs whatever `inner_cmd` resolves to and
exposes a stable `OMAC_*` + `/__omac__/` contract), so the remaining work is
to register the two new harnesses and supply per-harness bridges — bringing
the total to four supported agentic coders.

## What Changes

- Register the `codex` harness descriptor in `harnessRegistry()` (inner
  command `codex`; session-list kind; bridge = `.codex/hooks/`). Codex has
  no server mode: `SessionStart`-only lifecycle (its `Stop` hook is
  turn-scoped, not a session deactivation).
- Register the `copilot` harness descriptor in `harnessRegistry()` (inner
  command `copilot`; alias `co` — the standalone `copilot` exec, not
  `gh-copilot`; bridge = `.copilot/hooks/`). Copilot uses user-level hooks
  (`~/.copilot/`), not a repo-local `.github/hooks/` directory, and supports
  both `SessionStart` and `SessionEnd`.
- Add two new `SessionListKind` enum values (`SessionListCodex`,
  `SessionListCopilot`) and the corresponding `listCodex()` /
  `listCopilot()` session-listing functions.
- Extract the shared skills-manifest generator (previously inline in the
  OpenCode plugin) to `internal/manifest/Render()` so every bridge renders
  from the same activate-response shape.
- Ship native bridge scripts (shell, not MCP) for each harness:
  - `.codex/hooks/omac-bridge.sh` + `.codex/hooks.json`
  - `.copilot/hooks/omac-bridge.sh` + `.copilot/hooks/omac.json`
  Each bridge POSTs to `OMAC_CONTROL_BASE` `/__omac__/activate` and
  `/__omac__/deactivate`, injects the skills manifest into session context,
  and exposes per-skill base URLs under `OMAC_<MOUNT>_BASE` /
  `OMAC_G_<MOUNT>_BASE`, inert when `OMAC_CONTROL_BASE` is unset.

## Capabilities

### New Capabilities
- `codex-harness`: A registered `codex` harness descriptor + bridge wiring
  Codex CLI into the omac control plane and `OMAC_*` env contract —
  `SessionStart`-only lifecycle, per-directory skill activation/promotion,
  system-prompt skills manifest injection, and per-session skill base-URL
  injection.
- `copilot-harness`: A registered `copilot` harness descriptor (alias `co`)
  + bridge wiring GitHub Copilot CLI into the omac control plane —
  `SessionStart` + `SessionEnd` lifecycle, user-level hooks, same manifest
  and env contract.
- `shared-manifest-generator`: An extracted `internal/manifest/Render()`
  function that is the single source of truth for the skills-manifest text,
  consumed by every bridge (OpenCode plugin, Claude Code bridge, Codex
  bridge, Copilot bridge).

### Modified Capabilities
- `inner-harness`: The harness registry gains two new descriptors (`codex`,
  `copilot`) and two new `SessionListKind` values. The registry pattern and
  positional-subcommand UX are unchanged.
- `agent-bridge`: The bridge interface gains two concrete bridges (Codex,
  Copilot). The shared manifest source moves from the OpenCode plugin to
  `internal/manifest/Render()`; the OpenCode plugin is refactored to call
  it (no behavior change).

## Impact

- **Code (Go):** `internal/config/harness.go` (two new descriptors in
  `harnessRegistry()`, two new `SessionListKind` enum values),
  `internal/config/session_list.go` (`listCodex()` / `listCopilot()`),
  new `internal/manifest/` package (`Render()`), `.opencode/plugins/`
  refactored to consume `manifest.Render()`. Harness-agnostic core
  (`facade`, `supervisor`, `sandbox`, `registry`, `skillsource`, control
  plane) reused unchanged.
- **Bridges:** existing OpenCode plugin + Claude Code bridge retained
  unchanged in behavior; new Codex bridge assets under `.codex/`; new
  Copilot bridge assets under `.copilot/`.
- **Env/contract:** `OMAC_*` naming reused as-is; every bridge reads the
  same vars.
- **Skills & marketplace:** no per-skill code changes; skills already speak
  the `OMAC_*`/REST contract.
- **Docs:** README.md harness list, CREATING_A_SKILL.md skills-dirs section
  (deferred to a separate change).
- **Backward compatibility:** omitting the harness still defaults to
  `opencode`; existing `.opencode/` layouts and the OpenCode plugin keep
  working unchanged.

## Scope

In scope:
- Harness descriptors + session listing for `codex` and `copilot`
- Bridge scripts + hook registration for each harness
- Shared manifest generator extraction to `internal/manifest/`

Deferred to separate change:
- (none remaining — all items implemented in this change)

## Design

See `docs/superpowers/specs/2026-06-29-codex-copilot-backends-design.md`
for the full design. Key decisions:

- Two new `Harness` descriptors in `harnessRegistry()`
- Two new `SessionListKind` enum values
- Native bridge scripts (shell, not MCP) per harness (codex uses `.codex/hooks.json` per codex's convention; copilot uses `.copilot/hooks/omac.json` per copilot's user-level hooks convention)
- Codex: `SessionStart` only (Stop is turn-scoped, no deactivation)
- Copilot: `SessionStart` + `SessionEnd`, user-level hooks (not
  `.github/hooks/`)
- Shared manifest generator extracted to `internal/manifest/`
- Copilot alias is `co` (not `gh-copilot` — exec is standalone `copilot`)
