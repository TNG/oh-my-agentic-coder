# Design: herdr pane bridge

## Context

herdr sets `HERDR_ENV=1`, `HERDR_PANE_ID`, `HERDR_BIN_PATH` (and `HERDR_SOCKET_PATH`) on every pane process. Agents report via the herdr CLI:

```
"$HERDR_BIN_PATH" pane report-agent <pane_id> --source <src> --agent <label> \
    --state idle|working|blocked [--seq N] [--agent-session-id <id>] [-- <resume argv>...]
"$HERDR_BIN_PATH" pane release-agent <pane_id> --source <src> --agent <label> [--seq N]
```

Key herdr rules this design must honor:

- `--source` must be stable, unique, and NOT prefixed `herdr:` (reserved for herdr's own integrations). omac uses `omac`.
- `--agent` is the user-visible name; it must be the integrator's own name, not a name herdr already knows (`opencode`, `claude-code`, …). omac uses `omac`.
- `--seq` must be monotonically increasing across process restarts; herdr drops non-increasing seqs as stale. A UnixNano timestamp satisfies this, with a guard for a rewound clock (`lastSeq+1`).
- A resume argv is only accepted from a source that currently holds the pane (claim first via `report-agent`); the first word must be a plain PATH command, no apostrophes/control chars, ≤64 args, ≤8 KiB.
- Reports must be sent in the background with short timeouts, failures ignored, only the latest state sent; the pane must be released on exit (herdr has a screen-prompt safety net if an agent dies without releasing).

## Why host-side (the trust model)

omac's env stripping is a feature: nothing `HERDR_*` reaches the sandbox, so herdr's in-harness integrations are inert inside it. The temptation is to grant the socket + env into the sandbox for "proper" integration — but the herdr socket API can drive *other* panes (send input). Granting it hands the sandboxed agent control of every sibling pane. So the bridge runs in the omac process, on the host, and is the sole reporter. The trade-off: state is a heuristic, not a precise signal.

## State heuristic

The bridge polls a Probe every 2 s. The probe is stat-only (`internal/herdr/watch.go`): opencode → session db + `-wal` + `-shm` (WAL writes land in the sidecar); claude-code → the per-project session directory, entry-stamped individually. Any mtime/size/existence delta is activity.

- Launch → `working` (the launch itself is activity; the probe's first call is a baseline).
- Store change → `working` (refreshes the quiet timer; a streaming turn keeps the session from aging out mid-turn).
- Working + no change for 60 s → `idle` (the harness is back at its prompt; 60 s is generous on purpose so long builds do not flap the state).
- Never `blocked`: no precise host-side signal exists, and a false `blocked` is worse than herdr's screen-based detection.

Reports are coalesced: a report is only sent when the state, or the session id, differs from what was last reported.

## Session id and resume

The id is resolved most-authoritative first: an explicit resume (`omac continue -s <id>` / `omac resume`) knows it up front; opencode self-reports the live session id through omac's control plane during the run; claude-code fresh sessions are discovered by the probe itself (the new `<id>.jsonl` file name is the id). Ids are validated (`ValidSessionID`: omac's session id charset, no leading punctuation, `sidechain-` rejected — those are claude sub-agent sessions the user cannot resume) before they are ever embedded in a resume command.

Resume argv: `omac continue [harness] -s <id>` — the same hint omac prints at exit. herdr restores the pane through omac, so the session re-enters the sandboxed launch path and the skill-bundle drift check runs again at restore time. Failure mode: if the drift check fails at restore, omac errors as usual (herdr shows the pane with omac's message); documented in `docs/advanced/herdr.md`.

## Fail-soft contract

Every bridge call runs with a 2 s timeout in the bridge goroutine, output discarded (the inner harness owns the terminal). Errors are logged (verbose only) and ignored; the report content is recorded as sent even on error so an identical report is not retried — the next state change goes out regardless. A missing or hung herdr binary cannot affect launch or exit code. Close releases the pane and is idempotent; a second call returns immediately. If `Start` never started the loop (no probe), `Close` is a no-op rather than a deadlock.

## Config

`herdr.enabled` in the launcher config, `*bool` default-on (the repo's pattern for default-on switches). Global-only: `stripSecurityFields` zeroes it out of project-local configs, same as `sandbox.*` — the bridge runs on the host, so a project must not be able to flip it.
