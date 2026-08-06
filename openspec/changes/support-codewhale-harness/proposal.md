## Summary

Add CodeWhale (`codewhale`) as a supported harness in omac, following the
same declarative pattern used for OpenCode, Claude Code, Codex, Copilot, and
Pi — plus a new *file-based* briefing-delivery mechanism for harnesses that
load always-on context only from workspace files.

## Motivation

omac's harness registry is built for extensibility: adding a new agentic
coder is a matter of registering one descriptor, not editing call sites.
CodeWhale (a Rust CLI, npm package `codewhale`, multi-provider — DeepSeek,
OpenAI, Anthropic, OpenRouter, …) is a natural sixth harness. The Go core is
already harness-agnostic, so the work is: register the descriptor, teach omac
how CodeWhale enumerates sessions, and — because CodeWhale exposes neither a
system-prompt flag (like Claude/Codex) nor a plugin/env hook (like OpenCode/
Pi/Copilot) — add a delivery path that writes the sandbox briefing into a
workspace file CodeWhale loads.

## What Changes

- Register the `codewhale` harness descriptor in `harnessRegistry()` (inner
  command `codewhale`; config home `~/.codewhale` via `CODEWHALE_HOME`;
  `--continue` / `resume <id>` session args; multi-provider, so no
  auto-forwarded auth keys; `SkillsBase = SharedSkillsBase` because CodeWhale
  reads workspace `.agents/skills`, not a `.codewhale/skills`).
- Add a `BriefingFileFunc` field to `config.Harness`: a delivery mechanism
  that writes the briefing into the workdir for harnesses that load context
  only from the workspace tree. CodeWhale writes
  `.codewhale/rules/omac-sandbox-briefing.md` — every `.md` under
  `.codewhale/rules/` is loaded additively and unconditionally, unlike the
  first-found `.codewhale/instructions.md` which any `AGENTS.md`/`CLAUDE.md`
  shadows.
- The launcher (`omac start` / `omac serve`) writes the briefing file before
  exec, adds its path to `<workdir>/.git/info/exclude` (so an agent's own
  `git add -A` never commits it; persists across SIGKILL), and removes the
  file on clean exit.
- Add a `SessionListCodewhale` enum value and `listCodewhale()` — CodeWhale
  stores sessions in SQLite at `~/.codewhale/state.db` (the `threads` table);
  the backend reads it read-only, filters by `cwd`, drops archived threads.

## Capabilities

### New Capabilities
- `codewhale-harness`: A registered `codewhale` harness descriptor with
  file-based briefing delivery and SQLite session listing.
- `file-briefing-delivery`: `Harness.BriefingFileFunc` — a third briefing
  mechanism alongside `SystemContextArgs` (flag) and `BriefingEnvFunc`
  (env+tmp file), for harnesses that only read workspace instruction/rules
  files. Also relevant to #171 (harnesses that position system text
  themselves are immune to double-injection).

### Modified Capabilities
- `inner-harness`: The registry gains one new descriptor (`codewhale`), one
  new `SessionListKind` value, and one new optional descriptor field
  (`BriefingFileFunc`). The registry pattern and positional-subcommand UX are
  unchanged.

## Impact

- **Code (Go):** `internal/config/harness.go` (descriptor + `BriefingFileFunc`
  field + `SessionListCodewhale`), `internal/session/session.go`
  (`listCodewhale()` + `codewhaleDBPath()` + dispatch + `tableColumns()`
  helper), `internal/cli/briefing.go` + `start.go` + `serve.go`
  (`BriefingFileFunc` wiring + `gitExcludeBriefing` + `removeBriefingFile`).
- **Env/contract:** `OMAC_*` naming reused as-is.
- **E2e tests:** new harness config in `internal/e2e/harnesses.go`
  (`codewhaleConfig()`) + pinned version in `versions.go` + matrix wiring in
  `.github/workflows/e2e.yml` and `e2e-smoke.yml`.
- **Docs:** README.md harness list, docs/HARNESS_COMPAT.md, CREATING_A_SKILL.md
  (+ its bundled copy).
- **Backward compatibility:** omitting the harness still defaults to
  `opencode`; existing layouts and harnesses keep working unchanged.

## Scope

In scope:
- Harness descriptor + file-based briefing delivery for `codewhale`
- SQLite session listing (`listCodewhale`)
- E2e test config + pinned version + CI matrix
- Documentation updates
- Unit tests

Deferred / not verified:
- The macOS exclusion is precautionary (by analogy with codex's Rust
  HTTP-client × Seatbelt incompatibility) and UNVERIFIED for codewhale — one
  manual `E2E: drift` dispatch settles it.
- The model-calling e2e tiers are source-derived, not yet confirmed against a
  live gateway run.
