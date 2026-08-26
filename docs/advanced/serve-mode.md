---
title: Serve mode (OpenCode Desktop)
description: Using omac serve with OpenCode Desktop.
---

:::caution[Experimental]
Multi-project switching is not yet implemented. Single-project use with `--workdir` works today.
:::

See [omac serve](../usage/cli.md#omac-serve) for the command reference. This page covers the practical setup details that don't fit there.

## How to use it today

```bash
omac serve opencode --workdir ~/my-project
```

omac automatically launches `opencode serve` on port 4096 and handles authentication. OpenCode Desktop connects to it there by default — no additional port configuration is needed.

## Start order matters

Start `omac serve` **before** opening OpenCode Desktop. If you open Desktop first and then start `omac serve`, Desktop fails to load configured models and must be restarted. This is a known issue ([#252](https://github.com/TNG/oh-my-agentic-coder/issues/252)).

## Switching projects

When you switch to a different project in OpenCode Desktop, omac is not notified — Desktop does not yet call omac's activation API. Skills from the new project will not be active. The workaround is to restart `omac serve --workdir` pointing at the new project.
