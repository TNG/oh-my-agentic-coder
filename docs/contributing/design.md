---
title: Design decisions
description: The WHY behind omac's architecture
---

## Key decisions

### OS sandbox primitives, not a custom layer

omac delegates all filesystem, network, and process isolation to a sandbox backend — an OS-level program that builds the sandbox. omac's built-in backend re-executes omac itself to drive Seatbelt on macOS and bubblewrap + Landlock on Linux. 
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

### The sandbox definition is split over a trusted and an untrusted layer

The files that decide what a launch enforces (the launcher config's sandbox pointer, the sandbox profile, the learned network decisions) live in exactly two places: the user-global `~/.config/omac/` tree and the project-local `<workdir>/.omac/` directory. That split mirrors the trust split: the global layer is host-maintained and sits outside every granted path, the project layer sits in the agent-writable workdir and gets no trust on sight.

- A project may steer the sandbox through exactly one channel: `sandbox.profile_name` in `<workdir>/.omac/config.yaml`, and it resolves only inside `.omac/` (or a leaf-named profile at a layer directory). Everything else a config could control — audit settings, facade env passthrough, the briefing — comes exclusively from the global layer.
- Every launch that loads project-local content requires one explicit approval (`--accept-project-config`). The approval records a digest **per file path** in the host-only *pin store* (`~/.config/omac/project-sandbox.json`); "pin" is the term for an approved digest, distinct from the skill *approval record* (the code snapshot bundle_hash). A later launch aborts when a loaded file changed or disappeared since its pin, and the sandbox child re-verifies the pins before loading — the parent's check alone runs before several seconds of startup work that a leftover process could exploit.
- Defense is layered, from the inside out: the deny rules mask `.omac` for the running session (kernel mask on Linux, subpath rules on macOS); the pins detect anything the masks cannot prevent on macOS (a directory-entry replacement into the writable workdir); the human approval makes the first use of planted or committed project content a deliberate choice. What voids the pin guarantee on macOS is a profile — with grants over `~` or `~/.config` — approved by its user; the launch-time profile warning flags those grants as HIGH.
- Learned network decisions never live in a project directory; they are keyed under `~/.config/omac/learned/` (project profiles) or next to the global profile, and written by the sandbox supervisor process from outside the sandbox.

The 0.9.0 launcher-template settings (`sandbox.default_profile`, `sandbox.profiles`) are rejected with a migration hint. They are parsed only as presence sentinels so the hint can name the user's old values; when 0.10.x has been out long enough that a 0.9.0 config can no longer be expected in use, delete the sentinel fields and `validateSandbox`, plus the migration hint tests that pin them.

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
│  OMAC_FACADE_TOKEN=<per-session bearer token>                       │
│                                                                     │
│  opencode / claude-code:                                            │
│    curl -H "X-Omac-Facade-Token: $OMAC_FACADE_TOKEN" \              │
│         "$OMAC_SLACK_BASE/api/chat…"   # always TCP                  │
└─────────────────────────────────────────────────────────────────────┘
```
