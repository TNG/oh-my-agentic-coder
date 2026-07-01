## 1. Harness descriptors (Go core)

- [x] 1.1 Add `codex` descriptor to `harnessRegistry()` (inner `codex`; session-list kind; bridge = `.codex/hooks/`)
- [x] 1.2 Add `copilot` descriptor to `harnessRegistry()` (inner `copilot`; alias `co`; session-list kind; bridge = `.copilot/hooks/`)
- [x] 1.3 Add `SessionListCodex` and `SessionListCopilot` enum values
- [x] 1.4 Implement `listCodex()` and `listCopilot()` session-listing functions

## 2. Shared manifest generator

- [x] 2.1 Extract the skills-manifest renderer to `internal/manifest/Render()`
- [x] 2.2 Refactor OpenCode plugin to call `manifest.Render()` (no behavior change)

## 3. Codex bridge

- [x] 3.1 Create `.codex/hooks/omac-bridge.sh` (POST to `/__omac__/activate`; manifest injection; `OMAC_<MOUNT>_BASE` aliases)
- [x] 3.2 Create `.codex/hooks.json` (`SessionStart` only — Stop is turn-scoped, no deactivate)
- [x] 3.3 Bridge inert when `OMAC_CONTROL_BASE` unset

## 4. Copilot bridge

- [x] 4.1 Create `.copilot/hooks/omac-bridge.sh` (POST to `/__omac__/activate` + `/__omac__/deactivate`; manifest injection; `OMAC_<MOUNT>_BASE` aliases)
- [x] 4.2 Create `.copilot/hooks/omac.json` (`SessionStart` + `SessionEnd`, user-level hooks)
- [x] 4.3 Bridge inert when `OMAC_CONTROL_BASE` unset

## 5. Design spec

- [x] 5.1 Write design spec at `docs/superpowers/specs/2026-06-29-codex-copilot-backends-design.md`

## 6. OpenSpec change proposal

- [x] 6.1 Create `openspec/changes/support-codex-copilot-harnesses/proposal.md`
- [x] 6.2 Create `openspec/changes/support-codex-copilot-harnesses/tasks.md`

## 7. Implemented in this change (previously deferred)

- [x] 7.1 Update `README.md` harness list (add codex + copilot)
- [x] 7.2 Update `CREATING_A_SKILL.md` skills-dirs section (add `.codex/`, `.copilot/`)
- [x] 7.3 Add `omac manifest` subcommand (hybrid: bridges keep jq, subcommand for debugging)
- [x] 7.4 Add `*_HOME` env override support (expanded to all 4 harnesses)
- [x] 7.5 Add pre-flight binary check (`exec.LookPath`) in runLaunch + runServe + doctor

## 8. Tests

- [x] 8.1 Harness descriptor tests (codex + copilot lookup, fields, session metadata)
- [x] 8.2 Session listing dispatch tests (codex + copilot dispatch, missing-store handling)
- [x] 8.3 Manifest generator tests (`Render()` with ready/pending/broken/global skills)
- [x] 8.4 Bridge script tests (codex + copilot: exists, runs, inert when omac absent)
