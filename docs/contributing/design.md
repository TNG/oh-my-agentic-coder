---
title: Design decisions
description: The WHY behind omac's architecture
---

## Key decisions

### OS sandbox primitives, not a custom layer

omac delegates all filesystem, network, and process isolation to a sandbox backend — an OS-level program that builds the sandbox. The default `builtin` backend re-executes omac itself to drive Seatbelt on macOS and bubblewrap + Landlock on Linux. 
A custom sandbox would duplicate what the OS already does and force omac to track kernel-level security guarantees itself. Instead, omac only configures what the backend exposes: which socket path to allow and which loopback TCP port to open.
omac assembles the built-in backend's launch command internally; there is no user-configurable launcher command.

### Sidecars run on the host, not inside the sandbox

Sidecars need resources the sandbox must not touch: the OS keychain, credentials, filesystem paths outside the workdir. Running them inside would mean granting the sandbox those resources or building a second inner sandbox. On the host they inherit the user context they need, and the trust boundary stays clean: the sandbox sees only the facade's loopback TCP port and Unix socket, never a sidecar port, credential, or host env var.

### omac routes to a skill only after it passes a health check

A freshly spawned sidecar needs a moment to bind its port and initialize before it can serve. So omac starts the sidecar, polls the skill's `health.path` until it returns 2xx, and only then lets the facade route requests to it. If a sidecar later crashes, requests to it return `503` (`X-Omac-Reason: sidecar-down`); omac does not currently restart it.

### Secrets stay in the OS keychain, never in files or sandbox env

Credentials in env vars, `.opencode/` files, or shell configs are readable by any process that can reach them — including agent-generated code running inside the sandbox. The OS keychain (macOS Keychain Services, Linux Secret Service) is protected by the login session. omac collects secrets once at `omac register`, stores them under `service = omac/<skill>`, and injects them at `omac start` into the sidecar process env only — held in memory for the run, zeroed on exit, never forwarded into the sandbox.

### Two transports: TCP loopback and Unix socket

The facade binds two transports: a loopback TCP port (`OMAC_<SKILL>_BASE`) and a Unix socket (`OMAC_SOCKET`). Skills should always use TCP — it works under every backend, including sandbox configurations where a `(deny network*)` rule blocks `connect(2)` on a Unix socket. The socket is the original transport, kept for compatibility; there is no case where a skill needs to prefer it.

```
curl -sS "${OMAC_SLACK_BASE}/api/chat.postMessage" -d '...'
```

### Explicit registration, not auto-discovery at start time

Silently starting any sidecar found under `.opencode/skills/` would let install scripts run and secrets be collected without the user's awareness. So omac refuses to start if a skill directory holds an `omac.yaml` that is not in `sidecar.json`. Registration is deliberate: `omac register <skill>` lets the user inspect and run the install script and answer each secret prompt. A `bundle_hash` guards against drift — if a skill's source changes after registration, `omac start` refuses until the user re-registers with `--force`. `--auto-register-skills` exists for CI where all values resolve without prompting, but is not the default.

### `omac.yaml` is separate from the marketplace's `meta.yaml`

An omac skill needs two files: `SKILL.md`, read by the agent, and `omac.yaml`, read by the omac runtime. omac keeps its settings in its own `omac.yaml` rather than extending the marketplace's `meta.yaml`, so the two stay owned by separate projects and evolve independently — omac never reads `meta.yaml` at all.

| File | Read by | Content |
|---|---|---|
| `SKILL.md` | Agent (in sandbox) | Name, description, when to activate, instructions |
| `omac.yaml` | omac runtime (host) | `command`, `mount`, `secrets`, `health`, `install_scripts` |
| `meta.yaml` | Marketplace only — not omac | Version, author, distribution metadata; present only if the skill is published |

### The facade is the single trust boundary

Sidecar ports are ephemeral and bound to `127.0.0.1`. They are never exposed to the sandbox. Every sandbox request goes through the facade, which strips the `/<skill>/` mount prefix, enforces per-skill body-size and timeout limits, and routes to the right sidecar. A compromised sandbox can at most send HTTP to the facade — it cannot reach a sidecar port, host env var, or the keychain. The facade also gives skills a stable, mount-rooted URL regardless of which ephemeral port a sidecar lands on. Because it proxies rather than buffers, streaming responses such as Server-Sent Events and WebSocket upgrades reach the agent in real time.

## Architecture diagram

```
┌──────────────────────────── Host (user) ────────────────────────────┐
│                                                                     │
│   ┌──────────────┐   ┌──────────────┐   ┌──────────────┐            │
│   │ sidecar A    │   │ sidecar B    │   │ sidecar C    │            │
│   │ slack        │   │ email        │   │ jira         │            │
│   │ 127.0.0.1:   │   │ 127.0.0.1:   │   │ 127.0.0.1:   │            │
│   │  41017       │   │  41029       │   │  41033       │            │
│   └──────▲───────┘   └──────▲───────┘   └──────▲───────┘            │
│          │                  │                  │                    │
│          │    ┌─────────────┴──────────────────┴──┐                 │
│          └────┤  omac facade (reverse proxy)      │                 │
│               │   Unix socket: bridge.sock        │                 │
│               │   TCP loopback: 127.0.0.1:<port>  │                 │
│               │   routes:                         │                 │
│               │     /slack/*   → sidecar A        │                 │
│               │     /email/*   → sidecar B        │                 │
│               │     /jira/*    → sidecar C        │                 │
│               └─────────────────┬─────────────────┘                 │
│                                 │ socket bind-mounted               │
│                                 │ TCP port explicitly allowed       │
└─────────────────────────────────┼───────────────────────────────────┘
                                  │
┌─────────────────────────────────┼─ Sandbox (builtin) ───────────────┐
│  OMAC_SOCKET=/tmp/omac-.../bridge.sock                              │
│  OMAC_SLACK_BASE=http://127.0.0.1:<port>/slack                      │
│                                                                     │
│  opencode / claude-code:                                            │
│    curl "$OMAC_SLACK_BASE/api/chat…"   # always TCP                 │
└─────────────────────────────────────────────────────────────────────┘
```
