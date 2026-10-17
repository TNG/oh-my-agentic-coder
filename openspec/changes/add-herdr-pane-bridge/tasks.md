# Tasks: add-herdr-pane-bridge

## 1. Bridge core (`internal/herdr`)

- [x] 1.1 Detect herdr pane env (`HERDR_ENV`, `HERDR_PANE_ID`, `HERDR_BIN_PATH`) with strict validation (absolute bin path, no flag-like/control-char pane ids)
- [x] 1.2 ValidSessionID: omac session-id charset, no leading punctuation, length cap, `sidechain-` rejection
- [x] 1.3 Bridge: Start/Close lifecycle, claim pane as agent `omac` source `omac`, background loop with 2 s report timeout, monotonic seq (UnixNano + rewound-clock guard), release on Close, no-op Close without Start
- [x] 1.4 State heuristic: launch → working, store change → working, 60 s quiet → idle, never blocked; coalesce unchanged reports; fail-soft on runner errors (record as sent, no retry spam)
- [x] 1.5 Resume argv attachment after `--` once the session id is known
- [x] 1.6 Stat-only FileProbe: files + dir entries, mtime/size/existence deltas, new-entry id extraction, baseline on first call

## 2. Wiring (`internal/cli`, `internal/config`, `internal/session`)

- [x] 2.1 `herdr.enabled` global-only launcher config (`*bool`, default on, zeroed by `stripSecurityFields`)
- [x] 2.2 Exported store-path helpers in `internal/session` (OpenCodeDBPath, ClaudeProjectDir)
- [x] 2.3 `maybeHerdrBridge`: config/harness/env gating, per-harness probe (opencode db + wal/shm; claude project dir + JSONL id extraction), session-id resolution (explicit > control-plane report > probe discovery), verbose logf
- [x] 2.4 `herdrResumeArgv`: `omac continue [harness] -s <id>` honoring the continue-hint token
- [x] 2.5 Start/Close around the sandbox exec in `runLaunch`
- [x] 2.6 `omac doctor`: bridge enabled/disabled + in-pane detection line

## 3. Tests

- [x] 3.1 `internal/herdr`: Detect env matrix, ValidSessionID, IDsFromJSONL, FileProbe baseline/change/new-id/vanish/quiet
- [x] 3.2 `internal/herdr` Bridge with injected runner/clock/probe: first report claims pane working, resume argv after `--`, no duplicate unchanged reports, working→idle→working, fail-soft + no retry, seq monotonic incl. rewound clock, Close releases exactly once, Close without Start no-ops
- [x] 3.3 `internal/cli`: herdrResumeArgv per harness + invalid ids, maybeHerdrBridge nil-cases (disabled config, outside pane, unsupported store) and on-cases (opencode, claude-code)

## 4. Documentation

- [x] 4.1 `docs/advanced/herdr.md`: what the bridge reports, why host-side, limitations (heuristic state, no blocked, PATH requirement for restore, restore-time drift check)
- [x] 4.2 `docs/configuration.md`: `herdr.enabled` in the launcher config sample + global-only list

## 5. Finalization

- [x] 5.1 `go build ./...`, `go vet`, full package tests green
- [x] 5.2 `openspec validate add-herdr-pane-bridge --strict` (openspec CLI not installed on the contributing machine; the change was validated by hand against the schema of the merged `native-sandbox` change — same file layout, requirement/scenario structure)
