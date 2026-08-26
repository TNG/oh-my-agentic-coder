---
title: Configuration
description: omac configuration files and options
---

## Config files

| File | Purpose | Written by                            |
|---|---|---------------------------------------|
| `oh-my-agentic-coder.yaml` | Launcher config: sandbox runtime selection, facade tuning, audit settings | User                                  |
| `sandbox-profiles/default.json` | Sandbox grants: which filesystem paths, network hosts, and env vars the agent can access | `omac start` (first run scaffolds it) |
| `sandbox-profiles/default.pages.json` | Permanent allow/deny network decisions made via the prompt dialog | Network prompt dialog (user answers)  |
| `sidecar.json` | Skill registry: names, directories, bundle hashes, declared secrets | `omac register` / `omac deregister`   |
| `skill-config.yaml` | Non-secret per-skill fields: API base URLs, region names, feature flags | `omac register` / `omac config`       |

All files live under `~/.config/omac/` (user-global) or `<workdir>/.opencode/` (workdir-local). Where both exist, workdir wins.

## Launcher config

The launcher config selects which sandbox runtime to use and tunes a few operational settings. None of this controls what the agent is allowed to access — that is the sandbox profile (see below).

```yaml
sandbox:
  default_profile: builtin          # which sandbox runtime: builtin (default), nono (deprecated), nono-netprofile (deprecated), no-sandbox-debug (debugging)
  profiles: { }                     # custom runtime definitions (deprecated); leave empty unless you need a non-standard sandbox command
facade:
  idle_timeout_secs: 300            # close idle HTTP keep-alive connections after N seconds; does not end the session
  max_body_bytes: 10485760          # 10 MB request body cap
  base_env_passthrough: [PATH, HOME, USER, LANG, LC_ALL, LC_CTYPE, TMPDIR]
audit:
  enabled: true                     # security audit trail (default on)
  path: ""                          # "" uses the platform default path (see below)
  syslog: false                     # also mirror events to the system log (Unix)
  strict: false                     # fail-closed: abort the run if a log write fails
cache:
  scope: global                     # tool cache sharing: global (default), config, or workdir; see Cache
```

**`cache.scope`** controls how widely omac's isolated tool cache is shared between projects. It can be overridden per session with `--cache-scope`. See [Cache](./advanced/cache.md) for details.

**`idle_timeout_secs`** controls how long idle HTTP keep-alive connections to the facade are held open. It does not end the session — the agent and sandbox keep running regardless.

## Sandbox grants

The sandbox grants file (`~/.config/omac/sandbox-profiles/default.json`) controls what the agent is actually allowed to access — filesystem paths, network mode, and environment variables. This is separate from the launcher config above, which only selects which sandboxing technology to use.

omac scaffolds this file on first `omac start`. Key fields:

| Field | Type | Default | What it controls |
|---|---|---|---|
| `filesystem.deny` | `string[]` | `[".env", "*.key", "*.pem"]` | Blocks files inside granted directories by name or glob |
| `network.mode` | `string` | `"filtered"` | `filtered` (prompt for unknown hosts), `blocked` (no outbound at all), `open` (unrestricted) |
| `environment.allow_vars` | `string[]` | see scaffolded file | Env vars passed into the sandbox; everything else is stripped |
| `filesystem.protected_paths` | `string[]` | `["~/.ssh", "~/.gnupg", ...]` | Paths that remain blocked even if a broader grant would cover them |

See [Security model → Sandbox access reference](./security.md#sandbox-access-reference) for the full list of what the agent can and cannot access.

### Opening a port

To let the agent reach a local service, add the port to `network.open_port` in the sandbox grants file (`~/.config/omac/sandbox-profiles/default.json`):

```json
"network": { "open_port": [3000] }
```

On Linux this also permits outbound connections to that port on any host — Landlock cannot scope a port to localhost — so keep the list short. `omac doctor` and `omac provenance --check` flag every numeric `open_port`.

### Java and Node dependency downloads

Java (Maven/Gradle) and Node/npm do not reliably route their package downloads through a proxy on their own, so their downloads can fail inside the sandbox. To fix this, add `jvm`, `node`, or both to `network.proxy_injection` in the sandbox grants file, and omac configures those toolchains to use its proxy.

Node injection requires Node ≥ 22.21.0 (22.x line) or ≥ 24.5.0; on older versions it is skipped and downloads may still fail.

## Audit trail

omac logs every security-relevant action to an append-only file: process launches, network decisions, secret injections. The file is outside the sandbox so the agent cannot tamper with it.

| Platform | Default path |
|---|---|
| Linux | `~/.local/state/omac/audit/audit.jsonl` |
| WSL2 | `~/.local/state/omac/audit/audit.jsonl` (same as Linux; `~` is the WSL Linux home, not `C:\Users\…`) |
| macOS | `~/Library/Logs/omac/audit/audit.jsonl` |

| Flag | Effect |
|---|---|
| `--no-audit` | Disable the audit trail entirely |
| `--audit-strict` | Fail-closed: abort if the log cannot be opened or a write fails mid-session |
| `--audit-log <path>` | Write the log to `<path>` instead of the default |

By default, a write error emits a warning and the run continues.

## Corporate proxy

If your network routes outbound traffic through a corporate proxy, omac picks it up automatically from `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` in your shell environment — you do not need to configure anything. omac's own network filtering runs first, then allowed traffic is forwarded through the proxy.

If you need omac to use a different proxy than what your shell environment specifies, set `network.upstream_proxy` and `network.no_proxy` in the sandbox grants file.
