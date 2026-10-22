# Add herdr pane bridge (host-side)

## Why

omac runs fine inside [herdr](https://herdr.dev) panes, but the two tools currently work against each other:

1. **Restore bypasses the sandbox.** herdr restores an opencode pane with `opencode --session <id>` and a claude pane with `claude --resume <id>` — bare harness commands on the host. After a herdr server restart, the session comes back *outside* the omac sandbox, with none of omac's launch-time checks applied. That is exactly the path omac exists to close.
2. **The run is untracked.** omac strips the environment before launching the inner harness, so herdr's own harness integrations never see `HERDR_*` inside the sandbox and cannot report. herdr falls back to screen-based detection: it works, but it cannot know the pane holds an omac-wrapped agent, and the restore command stays the harness-native one.

The fix has to live in omac itself, in the omac process, which sits on the host outside the sandbox and does see the herdr pane environment. Passing `HERDR_*` or the herdr socket into the sandbox instead would hand the sandboxed agent herdr's pane API (which can send input to *other* panes) — a direct violation of omac's threat model.

GitHub issue: #371.

## What Changes

- New `internal/herdr` package: detection of the herdr pane environment, a fail-soft bridge that claims the pane as an `omac` agent, reports a working/idle state derived from host-side session-store activity, reports the session id plus a resume command, and releases the pane when the inner command exits.
- Resume command is `omac continue [harness] -s <id>`, so a herdr restore re-enters through the sandboxed launch path (skill-bundle drift check and all other launch-time validation re-applied).
- Activity probing without parsing: a stat-based file probe watches opencode's session db (plus `-wal`/`-shm` sidecars) and claude-code's per-project session directory; new claude `.jsonl` files surface their id as the session id.
- New global-only launcher config `herdr.enabled` (default on); project-local configs cannot change it (same rule as `sandbox.*`: the bridge runs on the host).
- `omac doctor` reports whether the bridge is enabled and whether omac is running inside a herdr pane.
- Docs: `docs/advanced/herdr.md` + launcher-config reference.

## Capabilities

### New Capabilities

- `herdr-pane-bridge`: host-side herdr integration — pane detection, lifecycle reports, sandboxed resume command, activity heuristic, release on exit, and the fail-soft contract.

### Modified Capabilities

- Launcher config gains a global-only `herdr.enabled` switch (change captured in `herdr-pane-bridge`; there is no merged baseline spec for the launcher config yet).

## Impact

- **New code**: `internal/herdr` (herdr.go, watch.go), `internal/cli/herdrbridge.go` (wiring into the launch pipeline).
- **Changed code**: `internal/cli/start.go` (bridge start/close around the sandbox exec), `internal/cli/doctor.go` (bridge check), `internal/config/launcher.go` (`Herdr` section + `stripSecurityFields`), `internal/session/paths.go` (exported store-path helpers).
- **Runtime dependency**: the `herdr` binary at `$HERDR_BIN_PATH`, provided by herdr itself for pane processes. All bridge calls are backgrounded with short timeouts; failures are ignored and never affect the launch or its exit code.
- **Security**: no `HERDR_*` variables and no herdr socket reach the sandboxed process. The bridge is the only reporter, from the host side.
- **Platform**: none (host-side `stat` + `exec`; works wherever omac works).

## Out of Scope / Future Work

- The `blocked` state: omac has no precise host-side signal for a harness permission prompt, and a wrong "blocked" is worse than herdr's screen-based fallback. Deliberately not reported.
- In-sandbox passthrough for precise state: possible as an opt-in grant later, documented with the trust trade-off (it exposes the herdr socket to the agent).
- Harnesses beyond opencode and claude-code (codex etc.): needs a watchable host-side store per harness; the bridge stays off for them until one exists.
