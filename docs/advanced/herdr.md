---
title: Herdr panes
description: How omac reports its state to herdr and restores sessions inside the sandbox
---

[herdr](https://herdr.dev) is a terminal session manager that keeps each agent in its own pane, tracks whether an agent is working or idle, and can restore agents after a restart. omac runs fine inside herdr panes without any configuration. This page describes the optional host-side bridge that integrates the two.

## What the bridge does

When omac detects that it is running inside a herdr pane (herdr sets `HERDR_ENV`, `HERDR_PANE_ID`, and `HERDR_BIN_PATH` on pane processes), the omac process itself reports to herdr on the harness's behalf:

- **Pane identity** — the pane is claimed as an `omac` agent, so herdr's sidebar shows what is actually running instead of guessing from the screen.
- **Working / idle state** — omac watches the harness's session store (a few `stat` calls every two seconds, on the host side). Fresh writes mean a turn is streaming: *working*. A store that has been quiet for a minute means the harness is back at its prompt: *idle*.
- **Sandboxed resume** — omac tells herdr to restore the pane with `omac continue [harness] -s <session id>`. After a herdr server restart, the session comes back **inside the sandbox**, with omac's launch-time checks re-applied. This is the main reason the bridge exists: herdr's built-in restore commands (`opencode --session <id>`, `claude --resume <id>`) would start the harness directly on the host, bypassing the sandbox entirely.
- **Release** — when the inner harness exits, omac releases the pane so it does not linger as "omac working" in herdr's tracker.

The bridge supports opencode and claude-code in this first version. For other harnesses it stays off, and herdr's own screen-based detection continues to work.

All reports are best-effort: they run in the background with short timeouts, and a missing or hung herdr binary can never affect an omac launch or its exit code.

## Why it is host-side

omac deliberately strips the environment before launching the inner harness, so herdr's own harness integrations cannot report from inside the sandbox: they never see the `HERDR_*` variables and cannot reach the herdr socket.

The bridge runs in the omac process instead, which sits on the host outside the sandbox. Nothing herdr-related is passed into the sandbox. That is not just a side effect of the env stripping — it is a security property: the herdr socket API can drive *other* panes (send input to them), so it must stay outside the reach of the sandboxed agent.

One consequence: the bridge's state is a heuristic (is the session store being written to?). omac never claims herdr's "blocked" state, because a wrong "blocked" is worse than herdr's own fallback — herdr's screen-based detection covers the "agent is showing a permission prompt" case more precisely than a file watcher ever could.

## Limitations

- The state signal is store activity, so a long-running tool that writes nothing for over a minute (a big build, a test suite) is reported *idle* until the next write. The threshold is deliberately generous to limit the flapping.
- omac must be reachable as `omac` on the user's `PATH` for herdr's restore to work, since herdr runs the resume command as a plain command.
- Restore re-enters through `omac continue`, which re-applies omac's skill-bundle drift check and other launch-time validation. If those fail at restore time, the restore fails with omac's normal error message rather than silently degrading.
- herdr restore requires herdr 0.9.2 or newer.

## Configuration

The bridge is on by default and requires no configuration. It activates only inside herdr panes, and it can be disabled globally in the [launcher config](../configuration.md):

```yaml
# ~/.config/omac/config.yaml
herdr:
  enabled: false
```

This setting is global-only: a project-local config file cannot change it, because the bridge runs on the host with your full environment and must stay under your control, not the project's. `omac doctor` prints whether the bridge is enabled and whether omac is currently running inside a herdr pane.
