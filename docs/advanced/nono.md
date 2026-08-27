---
title: nono sandbox (deprecated)
description: Using nono as an alternative sandbox backend (deprecated).
---

:::warning[Deprecated]
nono was omac's original sandbox backend. The built-in sandbox replaced it and is the recommended option for all new setups.
:::

## Setup

1. Install nono — see the [nono installation guide](https://nono.sh/docs/cli/getting_started/installation).
2. Configure a nono sandbox profile that grants omac's facade socket and TCP port — see [nono profile authoring](https://nono.sh/docs/cli/features/profile-authoring). This is the JSON file with filesystem, network, and environment settings.
3. `omac register <skill>` once per skill.
4. Select the profile in `oh-my-agentic-coder.yaml`:

```yaml
sandbox:
  default_profile: nono   # or nono-netprofile
```

## Profiles

| Profile | What it does |
|---|---|
| `nono` | Standard nono invocation with your configured sandbox profile |
| `nono-netprofile` | Identical to `nono` with one extra flag: `--network-profile opencode`. This activates nono's built-in domain allowlist for opencode, restricting outbound traffic to the domains opencode needs. The allowlist is bundled with nono — no additional setup required. |

## macOS and nono-netprofile

When `nono-netprofile` is active, nono routes all outbound traffic through its own HTTP proxy. On macOS, this causes the OS kernel to block Unix socket connections inside the sandbox.

**For most users this is invisible**: omac exposes skills to the agent over TCP by default, and that transport is unaffected. Skills work normally.

**For skill authors**: omac exposes each skill over two transports — TCP (`$OMAC_<SKILL>_BASE`) and Unix socket (`$OMAC_<SKILL>_SOCKET_BASE`). If you write skill client code that explicitly uses the Unix socket form, it will fail on macOS under nono-netprofile. Always prefer the TCP form.

## Cache isolation (for custom nono profile authors)

omac redirects tool caches (`npm`, `pip`, `cargo`, …) to an isolated directory so the agent cannot read or write `~/.cache` — the directory where these tools normally store downloaded packages on your machine. However, with nono, the filesystem grants are controlled by your nono sandbox profile, not by omac. If your profile grants broad read/write access to `~/.cache`, the agent can bypass omac's redirect and reach it directly.

This only affects users who write their own nono sandbox profile. When authoring one, make sure it does not broad-grant `~/.cache`. See [Cache](./cache.md) for context on what omac's cache isolation does.
