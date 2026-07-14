## 1. Harness descriptor (Go core)

- [x] 1.1 Add `pi` descriptor to `harnessRegistry()` (inner `pi`; session-list kind; bridge = `.pi/extensions/`)
- [x] 1.2 Add `SessionListPi` enum value

## 2. Session listing

- [x] 2.1 Extend `list()` signature with `piRoot string` parameter
- [x] 2.2 Implement `listPi()` session-listing function (JSONL files under ~/.pi/agent/sessions/)
- [x] 2.3 Add dispatch case for `SessionListPi` in `list()`
- [x] 2.4 Implement `piSessionsRoot(h)` helper

## 3. Pi bridge

- [x] 3.1 Create `.pi/extensions/omac-bridge/index.ts` (POST to `/__omac__/activate`; manifest injection via `before_agent_start`; reads `OMAC_SANDBOX_BRIEFING`)
- [x] 3.2 Create `.pi/extensions/omac-bridge/README.md`
- [x] 3.3 Bridge inert when `OMAC_CONTROL_BASE` unset

## 4. E2e config

- [x] 4.1 Add `piConfig()` to `internal/e2e/harnesses.go`
- [x] 4.2 Add `pi` to `allHarnesses()` slice
- [x] 4.3 Add pinned version and model ID to `internal/e2e/versions.go`

## 5. Tests

- [x] 5.1 Harness descriptor tests (pi lookup, fields, session metadata)
- [x] 5.2 Session listing dispatch tests (pi dispatch, missing-store handling)
- [x] 5.3 Bridge file existence test

## 6. Documentation

- [x] 6.1 Update `README.md` harness list (add pi)
- [x] 6.2 Update `CREATING_A_SKILL.md` skills-dirs section (add `.pi/`)
- [x] 6.3 Update `docs/MULTI_DIR_DESKTOP.md` bridge table (add pi)
