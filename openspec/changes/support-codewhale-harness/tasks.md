## 1. Harness descriptor (Go core)

- [x] 1.1 Add `codewhale` descriptor to `harnessRegistry()` (inner `codewhale`; config home `~/.codewhale` via `CODEWHALE_HOME`; `SkillsBase = SharedSkillsBase`; multi-provider → empty `SandboxEnvAllow`)
- [x] 1.2 Add `BriefingFileFunc` field to `config.Harness` (workdir file-based briefing delivery)
- [x] 1.3 Session args: `ContinueArgs = ["--continue"]`, `ResumeByIDArgs = ["resume", id]` (top-level `resume` subcommand, not a `--resume` flag)
- [x] 1.4 Add `SessionListCodewhale` enum value

## 2. Briefing delivery + git safety

- [x] 2.1 CodeWhale `BriefingFileFunc` writes `.codewhale/rules/omac-sandbox-briefing.md`
- [x] 2.2 Wire `BriefingFileFunc` into `start.go` and `serve.go` (write before exec, remove on exit)
- [x] 2.3 `gitExcludeBriefing()` appends the path to `.git/info/exclude` (idempotent, SIGKILL-safe, preserves user entries)

## 3. Session listing

- [x] 3.1 Extend `list()` signature with `codewhaleDB string` parameter + dispatch case
- [x] 3.2 Implement `listCodewhale()` (read-only SQLite `threads` query, cwd filter, drop archived, epoch-seconds `updated_at`)
- [x] 3.3 Implement `codewhaleDBPath(h)` helper (`~/.codewhale/state.db`)
- [x] 3.4 Extract `tableColumns()` schema-gate helper (also refactors `listCopilotDB`)

## 4. E2e config

- [x] 4.1 Add `codewhaleConfig()` to `internal/e2e/harnesses.go` (config.toml `provider="openai"` + gateway `base_url` + `path_suffix` + `api_key_env`; `exec --auto` RunArgs; `SkillsBase ".agents"`)
- [x] 4.2 Add `codewhale` to `allHarnesses()` + darwin exclusion
- [x] 4.3 Add pinned version, version-env, and model ID to `internal/e2e/versions.go`
- [x] 4.4 Wire matrix + version dispatch input into `.github/workflows/e2e.yml` and `e2e-smoke.yml`

## 5. Tests

- [x] 5.1 Harness descriptor tests (lookup, fields, session metadata, config home, `WorkdirSkillsDir`)
- [x] 5.2 `BriefingFileFunc` + `gitExcludeBriefing` + `removeBriefingFile` tests
- [x] 5.3 Session listing tests (dispatch, missing-db, parse/filter against a temp `state.db`)
- [x] 5.4 Contract tests (`TestDeriveContractKnownTokens`, `TestHarnessHasServerMode`)

## 6. Documentation

- [x] 6.1 Update `README.md` harness list (add codewhale)
- [x] 6.2 Update `docs/HARNESS_COMPAT.md` (harness list + macOS-exclude note)
- [x] 6.3 Update `CREATING_A_SKILL.md` skills-dirs section + its bundled copy

## 7. Follow-ups (not in this change)

- [ ] 7.1 Verify the macOS exclusion (manual `E2E: drift` dispatch) and drop it if CodeWhale works under Seatbelt
- [ ] 7.2 Confirm the model-calling e2e tiers against a live gateway run
