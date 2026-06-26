# oh-my-agentic-coder — Design Document

- **Status:** Draft v1
- **Audience:** Engineers building and operating the TNG sandboxed agent-coding stack (OpenCode / Claude Code / future clients) on top of Nono or any other sandbox runtime.
- **Scope:** Mechanism for streaming arbitrary REST/HTTP(S) interfaces from the host into a sandboxed agent-coding environment through a single Unix-domain-socket facade, driven by skill-sidecar metadata.
- **Non-goals:** Defining a new marketplace protocol; replacing Nono; authoring concrete skills.

---

## 1. Executive summary

`oh-my-agentic-coder` (CLI name: `omac`) is a small Rust binary and on-disk convention that:

1. Lets marketplace skills ship an optional **sidecar** — a host-side HTTP service that holds secrets the sandbox is not allowed to see.
2. Exposes all such sidecars to the sandbox through a single **Unix-domain socket facade**. Each skill is mounted under a path prefix (`/<skill>/…`) on that socket. No TCP port is ever exposed to the sandbox.
3. Provides `omac register / deregister / list / start / doctor` commands so the user can curate which sidecars are active per workdir, with an explicit, user-inspectable install step for each skill.
4. Is **sandbox-runtime agnostic**: the concrete launch command (Nono today, something else tomorrow) and the inner agent command (OpenCode, Claude Code, a plain shell) are both configured as templated argv.

The design generalizes the pattern already used by `himalaya-email`, which today runs a hand-started HTTP service on `127.0.0.1:7823` and is reached by the sandbox over loopback. The facade replaces that with a Unix socket plus a registry-driven supervisor.

---

## 2. Motivation

Current state (observed in this repository):

- Sandboxed OpenCode runs via `tng-opencode`, which is `nono run --allow-cwd --profile tng-sandbox -- opencode` (`install.sh:172`).
- Skills live under `.opencode/skills/<name>/` (e.g. `.opencode/skills/himalaya-email/`).
- `himalaya-email` already depends on an **out-of-sandbox HTTP service** (`scripts/gpg-service.py`) listening on `127.0.0.1:7823` because the sandbox cannot reach `~/.gnupg/S.gpg-agent`. The user must start that service by hand.

Problems with the status quo:

- Each skill that needs host resources invents its own ad-hoc loopback service, port, and startup procedure.
- The sandbox must still be allowed to reach host loopback in general.
- Secrets (GPG passphrases, Slack tokens, Jira tokens, …) leak into env vars or config files the skill itself reads, which implies the sandbox or the agent sees them.
- There is no uniform way for the marketplace to ship "this skill needs a background helper."

The proposal: a facade that the sandbox reaches via a **bind-mounted Unix socket** only, behind which the facade multiplexes requests to per-skill sidecar processes that run **outside** the sandbox and **own all secrets**.

---

## 3. Goals / Non-goals

### 3.1 Goals

- **No TCP surface** from sandbox to host for skill traffic. Only a Unix socket file.
- **Secrets stay outside.** Sidecars run in the host user's context with their own env; the sandbox only sees the socket.
- **Marketplace-driven.** Skills declare their sidecar in `omac.yaml`; the existing marketplace / skill-installer workflow is enough to distribute them.
- **Explicit installs.** No skill may execute arbitrary install commands during `register`; users inspect and run install scripts themselves.
- **Runtime-agnostic.** Nono is just one sandbox profile. The launcher is templated argv.
- **Streaming-first.** Supports plain HTTP, chunked transfer, Server-Sent Events, and WebSocket upgrades end-to-end.
- **Per-workdir isolation.** Socket path and sidecar set are scoped to the workdir the user is starting `omac` in.
- **Graceful lifecycle.** Supervisor cleans up sidecars and the socket on sandbox exit.

### 3.2 Non-goals

- Cryptographic isolation between sidecars of the same workdir.
- Mutual TLS or peer-credential auth on the socket (future work, §21).
- Windows (non-WSL) support in v1.
- A general service mesh. This is a single-process reverse proxy.

---

## 4. Glossary

| Term | Meaning |
| --- | --- |
| **Facade** | The `omac` process that owns the Unix socket and proxies requests to sidecars. Also the supervisor. |
| **Sidecar** | A host-side HTTP server process spawned from a skill's metadata. Owns the secrets and talks to the real upstream (Slack, Jira, Gmail, …). |
| **Skill** | A marketplace package installed under `.opencode/skills/<name>/`. May include an optional sidecar. |
| **Sandbox runtime** | The binary that actually creates the sandbox (Nono today). Swappable. |
| **Inner command** | What runs inside the sandbox (OpenCode, Claude Code, `bash`, …). Configurable. |
| **Bridge socket** | The single Unix socket through which all sidecar traffic flows. Path: `${TMPDIR}/omac-<workdir-hash>/bridge.sock`. |
| **Subpath mount** | A skill's routing prefix on the bridge socket. Defaults to the skill name. |
| **sidecar.json** | Per-workdir registry file under `.opencode/sidecar.json`. |
| **Secret** | A named credential declared in `sidecar.secrets`, stored in the OS keychain, injected into the sidecar's env at start time, and never visible to the sandbox. |
| **Keychain** | OS-native secret store: macOS Keychain Services, Linux Secret Service (libsecret/GNOME Keyring/KWallet), or Windows Credential Manager. |

---

## 5. High-level architecture

```
┌──────────────────────────── Host (user) ────────────────────────────┐
│                                                                     │
│   ┌──────────────┐   ┌──────────────┐   ┌──────────────┐            │
│   │ sidecar A    │   │ sidecar B    │   │ sidecar C    │            │
│   │ slack        │   │ himalaya     │   │ jira         │            │
│   │ 127.0.0.1:   │   │ 127.0.0.1:   │   │ 127.0.0.1:   │            │
│   │  41017       │   │  41029       │   │  41033       │            │
│   └──────▲───────┘   └──────▲───────┘   └──────▲───────┘            │
│          │                  │                  │                    │
│          │    ┌─────────────┴──────────────────┴──┐                 │
│          └────┤  omac facade (reverse proxy)      │                 │
│               │                                   │                 │
│               │   Unix socket: bridge.sock        │                 │
│               │   routes:                         │                 │
│               │     /slack/*       → sidecar A    │                 │
│               │     /himalaya/*    → sidecar B    │                 │
│               │     /jira/*        → sidecar C    │                 │
│               └─────────────────┬─────────────────┘                 │
│                                 │ bind-mounted / allow-listed       │
└─────────────────────────────────┼───────────────────────────────────┘
                                  │
┌─────────────────────────────────┼─ Sandbox (Nono) ──────────────────┐
│                                 │                                   │
│   OMAC_SOCKET=/tmp/omac-.../bridge.sock                             │
│   OMAC_SKILLS=slack,himalaya,jira                                   │
│   OMAC_SLACK_BASE=http://127.0.0.1:<port>/slack                    │
│                                                                     │
│   opencode / claude-code:                                           │
│     curl --unix-socket "$OMAC_SOCKET" http://x/slack/api/chat…      │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

Key invariants:

- The sandbox never sees a TCP socket belonging to a sidecar.
- The sandbox never sees host environment (tokens, keys).
- The facade is the single trust boundary.

---

## 6. Components

### 6.1 `omac` CLI binary

Single Rust binary. Subcommands (detailed in §10):

- `omac register <skill>`
- `omac deregister <skill>`
- `omac list`
- `omac start [-- …inner args]`
- `omac continue [harness] [-- …inner args]`
- `omac resume [harness]`
- `omac doctor`
- `omac version`

Rust is chosen because:

- Nono is already Rust; easy code-sharing and dependency overlap (`tokio`, `hyper`, `tower`).
- A single statically-linked binary ships cleanly — the user runs one thing on the host.
- Strong story for Unix sockets, async HTTP, WebSocket upgrade proxying (`hyper`, `hyper-util`, `tokio-tungstenite`).
- First-class ecosystem for the secret/keychain path that the CLI needs (`keyring`, `rpassword`, `secrecy`, `zeroize`), with native macOS Keychain and Secret Service backends out of the box.
- Process supervision with `tokio::process`, signal handling (`tokio::signal::unix`), and deterministic cleanup via `Drop`.
- Cross-compilable to the same targets Nono already ships.

### 6.2 Facade (reverse proxy)

- `tokio` + `hyper` server bound to the bridge Unix socket.
- Per-request router strips the `/<skill>/` prefix and forwards to `http://127.0.0.1:<port>/<rest>`.
- Full support for:
  - `Transfer-Encoding: chunked`
  - `text/event-stream` (SSE) — no buffering, pass-through flush
  - `Connection: Upgrade` (WebSocket)
  - Arbitrary HTTP methods and bodies up to a configured max (`facade.max_body_bytes`, default 10 MiB, override per skill)

### 6.3 Supervisor

- Spawns each registered sidecar as a child process.
- Allocates an ephemeral TCP port per sidecar on `127.0.0.1` by binding `:0`, then closing and passing the port to the child via `SIDECAR_PORT` env var.
- Runs a bounded health-probe loop (`sidecar.health.path`, `initial_delay_ms`, `timeout_ms`) before routing traffic.
- Restart policy: exponential backoff (1s, 2s, 4s, … capped at 30s), max 5 consecutive failures before marking the skill `degraded` (route returns 503).
- On facade shutdown: `SIGTERM` all children, wait up to 5s, then `SIGKILL`.

### 6.4 Sandbox launcher

- Generic argv templating (§15).
- Expands `{{socket}}`, `{{inner_cmd}}`, `{{inner_args}}`, `{{workdir}}`.
- Execs the sandbox command; inherits stdio.

---

## 7. Skill metadata extensions

### 7.1 omac.yaml: a separate file from the marketplace's meta.yaml

omac reads its per-skill *runtime* metadata from `omac.yaml`, NOT
`meta.yaml`. The original design extended the marketplace's
`meta.yaml`, but the two pipelines (publishing vs. omac) ended up
wanting different schemas in the same file, so they're now decoupled.
A skill that wants to be both publishable and omac-managed ships both
files.

Independently, every skill also ships a `SKILL.md` at its root —
the [agentskills.io](https://agentskills.io/) standard discovery file.
omac never parses `SKILL.md` (it's part of the bundle hash but
otherwise opaque to the runtime); the agent does, using its YAML
frontmatter for progressive-disclosure activation. The three files
have non-overlapping responsibilities:

- `SKILL.md` — agent-facing: name, description, when to activate,
  prose instructions for using the skill, references, scripts.
- `omac.yaml` — runtime-facing: sidecar `command`, `mount`,
  `secrets:`, `config:`, `health`, `install_scripts:`.
- `meta.yaml` — marketplace publishing pipeline (out of scope here).

See [`CREATING_A_SKILL.md`](./CREATING_A_SKILL.md) §3 for the
SKILL.md ↔ omac.yaml split in full detail.

The omac.yaml has the same top-level surface as the marketplace
`meta.yaml` (`name, type, version, description, author, dependencies`)
plus an optional `sidecar` block:

```yaml
name: slack
type: skill
version: 1.2.0
description: Slack REST bridge.
author: tngtech
dependencies: []

sidecar:
  # Argv run by the facade to start the sidecar.
  # The facade sets SIDECAR_PORT in the child env.
  command: ["./scripts/slack-sidecar", "--port", "${SIDECAR_PORT}"]

  # Optional: subpath on the bridge socket. Defaults to skill name.
  mount: slack

  # Strict allowlist of host env vars the sidecar may inherit
  # from the invoking user's shell. Nothing else is forwarded.
  env_passthrough:
    - HTTPS_PROXY

  # Secrets the sidecar needs at runtime. `omac register` prompts
  # for each of these and stores them in the OS keychain. They are
  # injected into the sidecar process env at `omac start`.
  # See §16 for the full keychain model.
  secrets:
    - name: SLACK_BOT_TOKEN
      description: "Bot token (xoxb-…) for the Slack workspace."
      required: true
      pattern: "^xoxb-[A-Za-z0-9-]+$"
    - name: SLACK_APP_TOKEN
      description: "App-level token (xapp-…) for Socket Mode."
      required: false
      pattern: "^xapp-[A-Za-z0-9-]+$"

  # Liveness probe. Facade waits until this returns 2xx.
  health:
    path: /status
    initial_delay_ms: 200
    timeout_ms: 5000
    interval_ms: 500

  # Per-OS install scripts shipped inside the skill package.
  # Paths are relative to the skill root.
  install_scripts:
    macos: install/install.macos.sh
    linux: install/install.linux.sh
    wsl:   install/install.linux.sh

  # Informational. Used by `omac list`.
  protocols: ["http", "sse"]

  # Optional overrides.
  limits:
    max_body_bytes: 10485760
    idle_timeout_secs: 300
```

All fields under `sidecar` are optional except `command`. A skill without a `sidecar` block is a pure in-sandbox skill and is unaffected by `omac`.

### 7.2 JSON-Schema fragment

```json
{
  "$id": "https://tng/omac/sidecar.schema.json",
  "type": "object",
  "required": ["command"],
  "properties": {
    "command":         { "type": "array", "items": { "type": "string" }, "minItems": 1 },
    "mount":           { "type": "string", "pattern": "^[a-z0-9][a-z0-9-]*$" },
    "env_passthrough": { "type": "array", "items": { "type": "string" } },
    "secrets": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["name"],
        "properties": {
          "name":        { "type": "string", "pattern": "^[A-Z_][A-Z0-9_]*$" },
          "description": { "type": "string" },
          "required":    { "type": "boolean", "default": true },
          "pattern":     { "type": "string" },
          "default_from_env": { "type": "string" },
          "multiline":   { "type": "boolean", "default": false }
        }
      }
    },
    "health": {
      "type": "object",
      "properties": {
        "path":             { "type": "string", "default": "/status" },
        "initial_delay_ms": { "type": "integer", "minimum": 0, "default": 200 },
        "timeout_ms":       { "type": "integer", "minimum": 100, "default": 5000 },
        "interval_ms":      { "type": "integer", "minimum": 50, "default": 500 }
      }
    },
    "install_scripts": {
      "type": "object",
      "properties": {
        "macos": { "type": "string" },
        "linux": { "type": "string" },
        "wsl":   { "type": "string" }
      }
    },
    "protocols": { "type": "array", "items": { "enum": ["http", "sse", "websocket"] } },
    "limits": {
      "type": "object",
      "properties": {
        "max_body_bytes":   { "type": "integer", "minimum": 0 },
        "idle_timeout_secs":{ "type": "integer", "minimum": 1 }
      }
    }
  }
}
```

### 7.3 Example: retrofitting `himalaya-email`

```yaml
sidecar:
  command: ["uv", "run", "scripts/gpg-service.py", "--port", "${SIDECAR_PORT}"]
  mount: himalaya
  env_passthrough: [GNUPGHOME]
  health: { path: /status, initial_delay_ms: 500, timeout_ms: 3000 }
  install_scripts:
    macos: install/install.macos.sh
    linux: install/install.linux.sh
  protocols: [http]
```

Inside the sandbox the skill then calls:

```bash
curl --unix-socket "$OMAC_SOCKET" http://x/himalaya/status
```

instead of `http://127.0.0.1:7823/status`.

---

## 8. On-disk state

### 8.1 `.opencode/sidecar.json`

Per-workdir registry. Authoritative list of which skills' sidecars are active.

```json
{
  "version": 1,
  "registered": [
    {
      "name": "slack",
      "skill_dir": ".opencode/skills/slack",
      "bundle_hash": "sha256:…",
      "registered_at": "2026-04-22T08:15:00Z"
    },
    {
      "name": "himalaya-email",
      "skill_dir": ".opencode/skills/himalaya-email",
      "bundle_hash": "sha256:…",
      "registered_at": "2026-04-22T08:20:12Z"
    }
  ]
}
```

Write semantics:

- All mutations go through write-to-temp + `rename(2)` (atomic on POSIX).
- A `flock(2)` on `.opencode/sidecar.json.lock` serializes concurrent `omac` invocations.
- `bundle_hash` pins the entire skill source tree at register time
  (every meaningful file under `.opencode/skills/<name>/`, excluding
  runtime artifacts like virtualenvs, caches, `node_modules/`, VCS
  metadata). On `omac start`, if the current bundle hash differs,
  `omac` refuses to start unless `--accept-skill-changes` is passed
  (or the user re-registers with `omac register --force <skill>`).

### 8.2 Skill layout

Skills live in one of two source layers, both keyed by `<name>`.
Within each layer, omac honors two parallel naming conventions: the
agentskills.io-aligned `agents/skills` location (preferred for new
skills) and the legacy `opencode/skills` location.

1. **Workdir-local** (always scanned, in this order):
   1. `<workdir>/.agents/skills/<name>/`
   2. `<workdir>/.opencode/skills/<name>/`
2. **User-global** (only roots that exist on disk are scanned):
   1. `$XDG_CONFIG_HOME/agents/skills/<name>/`
   2. `$XDG_CONFIG_HOME/opencode/skills/<name>/`
   3. `~/.config/agents/skills/<name>/`
   4. `~/.config/opencode/skills/<name>/`
   5. `~/.agents/skills/<name>/` (legacy flat layout)
   6. `~/.opencode/skills/<name>/` (legacy flat layout)

`omac register <name>` resolves the source by trying these in order
and using the first hit. `.agents/skills` ranks above
`.opencode/skills` in every layer; workdir-local always wins over
user-global. Registration data (`sidecar.json`, `skill-config.yaml`,
OS keychain entries) is always per-workdir; the source layer only
controls **where the skill code lives**. `omac` does not manage
download or update — that remains the job of `skill-installer`.

The `skill_dir` field in `sidecar.json` records the exact location
the skill was registered from: a path relative to the workdir for
workdir-local skills (so the registry stays portable when the project
moves), or an absolute path for user-global skills.

### 8.3 Runtime state directory

On `omac start`, the facade creates:

```
${TMPDIR:-/tmp}/omac-<workdir-hash>/
├── bridge.sock          # 0600, owner = invoking user
├── facade.pid
├── ports.json           # {"slack": 41017, "himalaya-email": 41029}
├── pids/
│   ├── slack.pid
│   └── himalaya-email.pid
└── logs/
    ├── facade.log
    ├── slack.log
    └── himalaya-email.log
```

`<workdir-hash>` = first 12 hex chars of `sha256(abspath(workdir))`.

All files in this directory are created with `0600` / `0700` masks. On clean shutdown the directory is removed. On crash it is removed by the next `omac start` or `omac doctor`.

---

## 9. Environment contract (inside sandbox)

The facade binds two transports — Unix domain socket and 127.0.0.1
TCP — and surfaces both to the sandbox. The launcher injects these
variables:

| Variable | Value | Purpose |
| --- | --- | --- |
| `OMAC_BASE` | `http://127.0.0.1:<port>/` | Top-level TCP base URL. |
| `OMAC_HOST` | `127.0.0.1` | Components of OMAC_BASE for clients that build URLs. |
| `OMAC_PORT` | `<port>` | Same. |
| `OMAC_SOCKET` | Absolute path to `bridge.sock`. | Unix transport (fallback). |
| `OMAC_SKILLS` | Comma-separated list of active skill mounts. | Introspection. |
| `OMAC_<SKILL>_BASE` | `http://127.0.0.1:<port>/<mount>` | **Preferred** per-skill URL (TCP), without a trailing slash. |
| `OMAC_<SKILL>_SOCKET_BASE` | `http+unix://<pct-encoded-socket>/<mount>` | Per-skill Unix URL (fallback), without a trailing slash. |
| `OMAC_VERSION` | `omac` binary version. | Skill compatibility checks. |

Skill name → env suffix mapping: uppercase, `-` → `_`, non-alphanumerics stripped. `himalaya-email` → `OMAC_HIMALAYA_EMAIL_BASE` and `OMAC_HIMALAYA_EMAIL_SOCKET_BASE`.

**Why two transports.** On macOS, when nono runs in proxy mode (auto-
activated by any nono profile defining `custom_credentials`, by
`--network-profile`, `--allow-domain`, `--credential`, or
`--upstream-proxy`), Seatbelt installs `(deny network*)`. Unix-socket
`connect(2)` is classified as `network-outbound`, so the AF_UNIX form
becomes unreachable. The launcher passes `--open-port <tcp-port>` to
nono, which emits a more-specific allow rule that takes precedence
over the blanket deny. The Unix path remains valid in unsandboxed
runs and on Linux. Clients should prefer the TCP form.

### Client examples

```bash
# Preferred: TCP form, works under nono proxy mode on macOS.
curl -sS "${OMAC_SLACK_BASE}/api/chat.postMessage" -d '...'
```

```bash
# Fallback: Unix-socket form (works on Linux + on macOS without proxy mode).
curl -sS --unix-socket "$OMAC_SOCKET" http://x/slack/api/chat.postMessage
```

```python
# Python (TCP):
import os, requests
r = requests.get(f"{os.environ['OMAC_SLACK_BASE']}status")
```

```python
# Python (Unix):
import requests_unixsocket, os
s = requests_unixsocket.Session()
r = s.get(f"http+unix://{os.environ['OMAC_SOCKET'].replace('/', '%2F')}/slack/status")
```

```javascript
// Node (undici, TCP):
import { request } from "undici";
await request(`${process.env.OMAC_SLACK_BASE}status`);
```

```javascript
// Node (undici, Unix):
import { request } from "undici";
await request("http://x/slack/status", { socketPath: process.env.OMAC_SOCKET });
```

Skills document the specific endpoints their sidecar exposes; `omac` does not prescribe them.

---

## 10. CLI reference

All commands are idempotent unless noted. All accept `--workdir <dir>` (default: cwd) and `--config <path>` (default: `<workdir>/.opencode/oh-my-agentic-coder.yaml`, falling back to `~/.config/omac/config.yaml`).

### 10.1 `omac register <skill>`

1. Resolves `<skill>` against the layered source list (§8.2):
   workdir-local first (`.agents/skills/` then `.opencode/skills/`),
   then user-global (`agents/skills/` then `opencode/skills/` under
   the XDG config home, with a legacy `~/.agents/skills/` and
   `~/.opencode/skills/` flat fallback). Loads `omac.yaml` from the
   first layer that contains it.
2. Validates the `sidecar` block against the JSON-Schema. If the skill has no sidecar block, exits with code `2` and a clear message.
3. **Secret prompting** (new, see §16 for the keychain details):
   - For each entry in `sidecar.secrets`, checks whether a value already exists in the OS keychain under `service = omac/<skill>`, `account = <secret.name>`.
   - If not present:
     - If `default_from_env` is set and the matching env var is present in the user's shell, the value is taken from there (with a visible confirmation).
     - Otherwise, the user is prompted interactively. Input is masked (`rpassword`-style no-echo). Values are not logged or stored anywhere except the keychain.
     - `multiline: true` opens a `$EDITOR` session (or reads stdin until EOF in non-tty mode).
     - The value is validated against `pattern` if present; on mismatch, the user is reprompted up to 3 times.
   - If `required: false` and the user presses Enter on an empty prompt, the secret is marked absent (nothing is stored).
   - Values are written to the OS keychain immediately on accept (one keychain entry per secret).
   - `--secrets-from <file>` may be passed to read `KEY=VALUE` pairs from a file instead of prompting (the file is then deleted or the user is told to delete it; never stored).
   - `--reprompt-secrets` forces re-prompting even if values already exist in the keychain.
   - `--no-secrets` skips the keychain path entirely; the user commits to providing secrets via `env_passthrough` at start time (useful in CI).
4. Detects OS (`macos` / `linux` / `wsl`) and looks up `sidecar.install_scripts.<os>`.
5. Surfaces (never executes) the install script for the host OS:
   - Prints the absolute path of the install script.
   - Prints the recommended invocation: `bash <path>`.
   - Prints a reminder that omac will not run it.
   The script body itself is NOT dumped to the terminal; users inspect
   the file and run it themselves. If `omac.yaml` declares an install
   script for this OS but the file is missing on disk, a `[warn]` is
   emitted but registration proceeds.
6. Appends the skill to `.opencode/sidecar.json` (atomic rename), recording the list of secret **names** (never values) that were populated. If already present with a matching `bundle_hash`, is a no-op for metadata. If present with a different hash, prints a diff and updates on `--force`.

Exit codes: `0` success, `2` no sidecar block, `3` schema validation failed, `4` skill not installed, `5` registry write failed, `8` keychain access failed, `9` required secret refused by user.

### 10.2 `omac deregister <skill>`

Removes the entry from `sidecar.json`. Does **not** uninstall the skill and does **not** attempt to undo install-script side-effects. Prints the list of files/binaries the install script produced (from an optional `sidecar.install_artifacts` hint, if the skill provides one) so the user can clean up manually.

By default, secrets stored in the keychain are **retained** (so re-registering later is frictionless). Pass `--purge-secrets` to also delete every `omac/<skill>/<secret>` keychain entry. A one-line summary is printed either way (`kept 2 secret(s) in keychain` vs `deleted 2 secret(s) from keychain`).

Exit codes: `0` success (also if skill was not registered), `5` registry write failed, `8` keychain access failed.

### 10.2.1 `omac secrets <subcommand>`

Dedicated management for the keychain state of a registered skill. All subcommands take `<skill>` as a positional argument.

- `omac secrets list <skill>` — print the declared secret names, whether each is present in the keychain, and the timestamp of the last set. **Never** prints values.
- `omac secrets set <skill> <name>` — (re)prompt and store a single secret.
- `omac secrets unset <skill> <name>` — remove one secret from the keychain.
- `omac secrets import <skill> --from <file>` — bulk-load from `KEY=VALUE` file; fails if an entry is not declared in `sidecar.secrets`.
- `omac secrets export <skill>` — intentionally **not** provided. Values are write-only from `omac`'s perspective once stored.

Exit codes: `0` success, `3` unknown secret name, `8` keychain access failed.

### 10.3 `omac list`

Prints a table:

```
NAME            MOUNT         HEALTH   BINARY-PRESENT   LAST-REGISTERED
slack           /slack/       ok       yes              2026-04-22 08:15
himalaya-email  /himalaya/    ok       yes              2026-04-22 08:20
```

`HEALTH` is shown only when a facade is currently running for this workdir (via `pids/` lookup).

### 10.4 `omac start [-- …inner args]`

Full lifecycle. Detailed in §12.

Common flags:

- `--inner <cmd>` — override `sandbox.inner_cmd` from config.
- `--sandbox <profile>` — select a named sandbox profile from config (`sandbox.profiles.<name>`).
- `--no-sandbox` — run the inner command directly without a sandbox (dangerous; for debugging only).
- `--keep-running` — do not stop sidecars when the inner command exits (useful when iterating on sidecar development).
- `--accept-skill-changes` — tolerate `bundle_hash` drift.
- `--skip-secret-pattern` — do not enforce a secret's `pattern` against an `env_passthrough`-supplied value (escape hatch for an outdated pattern; the raw value is still passed through to the sidecar).
- `--verbose`, `--log-level <level>`.

### 10.4a `omac continue [harness] [-- …inner args]`

Runs the full `omac start` lifecycle (§12) but re-enters the most recent
session for the current workdir by appending the resolved harness's "continue"
inner flag (opencode and Claude Code: `--continue`). Accepts the same flags and
optional leading `[harness]` token as `start`. If the harness declares no
session support, it exits with a clear message (code `2`).

### 10.4b `omac resume [harness]`

Lists the recent sessions scoped to the current workdir, presents an
interactive numbered picker (index, relative time, title), and launches the
selected session through the `start` lifecycle with the harness's "resume by
id" inner flag (opencode `--session <id>`, Claude Code `--resume <id>`).

Sessions are read from each harness's own store via a per-harness strategy:

- **opencode** — parse `opencode session list --format json`, keep records
  whose `directory` is the workdir.
- **claude-code** — read `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl`;
  the id is the filename, the title is the latest `aiTitle` record (falling
  back to the first user message, then the id), the time is the latest record
  timestamp (falling back to file mtime). The lossy directory encoding is only
  a lookup hint — membership is confirmed against each file's embedded `cwd`.

Listing is best-effort: a missing CLI/store yields "no resumable sessions"
rather than an error. With non-interactive stdin, the list is printed with a
hint and no selection is made. Cancelling the picker exits without launching.

### 10.5 `omac doctor`

Runs sanity checks:

- `omac` version, OS detection, `$TMPDIR` writable.
- Each registered skill: meta valid, install artifact (if declared) present and executable, health-probe dry parse.
- Stale runtime directories for this workdir (offers `--fix` to remove them).
- Configured sandbox binary resolvable on `$PATH`.

### 10.6 Exit codes (global)

| Code | Meaning |
| --- | --- |
| `0` | success |
| `1` | generic failure |
| `2` | misuse / invalid arguments |
| `3` | configuration or metadata invalid |
| `4` | prerequisite missing (skill not installed, binary not built) |
| `5` | I/O error (registry, socket, fs) |
| `6` | sidecar failed health check before sandbox was launched |
| `7` | sandbox process exited with a non-zero code (exit code is propagated into the low 8 bits where possible) |

---

## 11. Registration lifecycle (sequence)

```
user: omac register slack
 ├─ read .opencode/skills/slack/omac.yaml
 ├─ validate sidecar block
 ├─ for each declared secret:
 │    ├─ already in keychain?     → skip (unless --reprompt-secrets)
 │    ├─ default_from_env hit?    → take value, confirm with user
 │    └─ else                     → masked prompt, validate pattern, store in keychain
 ├─ detect OS → select install_scripts.macos
 ├─ print: install-script path + "run it yourself" hint (no body dump)
 ├─ acquire .opencode/sidecar.json.lock (flock)
 ├─ read registry → append entry
 │   (name, skill_dir, bundle_hash, timestamp, declared_secret_names[])
 ├─ write registry.tmp → rename → registry
 └─ release lock
```

No sidecar process is spawned. Secret values are stored only in the OS keychain; only their names land in `.opencode/sidecar.json`. No file outside `.opencode/` and the keychain is touched.

---

## 12. Start lifecycle (sequence)

```
user: omac start -- --some --inner-arg

omac:
 1. Load config (§15) and .opencode/sidecar.json.
 2. Drift reconciliation (refuses to spawn anything until clean):
    a. Auto-deregister entries whose skill_dir no longer exists.
       Silent except for an [info] log + a hint about how to purge
       leftover secrets/config (`omac deregister --purge-secrets
       --purge-fields <skill>`). Stored values are KEPT to make
       accidental `rm -rf` recoverable.
     b. Refuse if any directory in any source layer (workdir-local
        .agents/skills/ + .opencode/skills/, or any of the user-global
        roots — see §8.2) contains a omac.yaml but is not in the
        registry; print the exact `omac register <skill>` command for
        each one. Names duplicated across layers are deduplicated
        (workdir wins, .agents wins over .opencode).
    c. For each registered skill, recompute bundle_hash; if it
       differs from the stored value, refuse unless
       --accept-skill-changes is set. (`omac register --force <skill>`
       is the supported way to re-register on intentional changes.)
    d. For each registered skill, ensure every required `config:`
       field resolves: stored value > spec default > default_from_env
       in the host shell. Refuse with the list of missing names if
       not, suggesting `omac register --reprompt-fields <skill>`.
 3. For each registered skill, read its declared secrets from the OS keychain
    (service = "omac/<skill>", account = <secret.name>).
    - Missing required secrets → abort with exit 9 and a clear prompt:
      `omac secrets set <skill> <name>` or `omac register <skill> --reprompt-secrets`.
    - Missing optional secrets → skipped silently.
    Secrets are held only in process memory of omac, never written to disk,
    and are wiped on exit (see §16.6).
 4. Create ${TMPDIR}/omac-<hash>/ with logs/ pids/.
 5. For each skill:
      a. bind TCP :0 on 127.0.0.1 → get ephemeral port → close.
      b. record port in ports.json.
      c. spawn sidecar.command with:
           env = allowlisted(host_env, sidecar.env_passthrough)
               + keychain_secrets(skill)          # <-- injected here
               + {SIDECAR_PORT=<port>, SIDECAR_SKILL=<name>,
                  OMAC_WORKDIR=<workdir>}
           cwd = <skill_dir>
           stdio = pipe → logs/<skill>.log
           The secret values are passed to the child via its env only;
           they are never passed as argv (so they do not appear in `ps`).
 6. Health-wait each sidecar (bounded by health.timeout_ms + initial_delay_ms).
    Any failure here → kill already-started sidecars, remove runtime dir, exit 6.
 7. Open bridge.sock (mode 0600).
 8. Mount routes: "/<mount>/*" → "http://127.0.0.1:<port>/".
 9. Build sandbox argv from config template (§15) and exec sandbox.
    Secret values are NEVER forwarded into the sandbox environment.
10. Propagate SIGINT/SIGTERM from omac to sandbox; on sandbox exit:
      a. SIGTERM sidecars, wait ≤5s, SIGKILL stragglers.
      b. Unlink bridge.sock.
      c. Zero out in-memory secret buffers.
      d. Remove runtime dir (unless --keep-running or a sidecar is still alive).
      e. Exit with sandbox's exit status (clamped to 0-255; 7 on abnormal).
```

Step 5c detail: `env_passthrough` is a strict allowlist. Anything not listed is **not** forwarded. `PATH`, `HOME`, `USER`, `LANG`, `LC_*`, `TMPDIR` are always forwarded (operational minimum) but this base set is itself documented and configurable via `facade.base_env_passthrough` in config. The `keychain_secrets(skill)` map takes precedence over `env_passthrough` collisions: if the user had `SLACK_BOT_TOKEN` in their shell *and* in the keychain, the keychain value wins (and a warning is logged the first time this happens).

---

## 13. Proxy semantics

### 13.1 Routing

- Incoming request path must match `^/(?P<mount>[a-z0-9][a-z0-9-]*)/(?P<rest>.*)$`.
- `mount` is looked up in the routing table (built at start-up from `sidecar.json` + per-skill `sidecar.mount`).
- Upstream URL is `http://127.0.0.1:<port>/<rest>` preserving query string.
- Unknown mount → `404` with body `omac: unknown skill mount '<mount>'`.
- Requests to `/` → facade status JSON: `{"skills": [...], "version": "…"}`.

### 13.2 Header hygiene

- `Host` on the upstream request is set to `127.0.0.1:<port>`.
- Hop-by-hop headers (`Connection`, `Keep-Alive`, `Proxy-*`, `TE`, `Trailers`, `Transfer-Encoding`, `Upgrade` unless upgrading) are not forwarded.
- `X-Forwarded-For` and `X-Forwarded-Prefix: /<mount>` are added on the upstream side so sidecars can generate correct absolute URLs if they need to.
- The facade does **not** inject any of the host's auth headers. Auth is the sidecar's job.

### 13.3 Bodies & streaming

- Request and response bodies are streamed (`hyper::Body`), not buffered, except when `Content-Length` ≤ 64 KiB, where a one-shot copy is permitted for latency.
- `text/event-stream` responses are detected by `Content-Type`; the facade disables any chunk coalescing and flushes on every upstream frame.
- `Transfer-Encoding: chunked` both ways is transparent.
- `max_body_bytes` (per skill, else facade default) is enforced; exceeding it yields `413 Payload Too Large` and the upstream request is cancelled.

### 13.4 Upgrades (WebSocket)

- On requests carrying `Upgrade: websocket` and `Connection: Upgrade`, the facade opens an upstream TCP connection, forwards the handshake, then splices the two streams bidirectionally until either side closes.
- `Sec-WebSocket-*` headers are passed through verbatim.

### 13.5 Timeouts

- Connect timeout to upstream: 2 s.
- Idle timeout: `sidecar.limits.idle_timeout_secs` (default 300).
- Header read timeout: 15 s.
- On timeout, the facade returns `504 Gateway Timeout`.

### 13.6 Error mapping

| Situation | Facade response |
| --- | --- |
| Sidecar returns 5xx | Proxied as-is. |
| Upstream connection refused / sidecar dead | `503 Service Unavailable` with `X-Omac-Reason: sidecar-down`. |
| Timeout | `504 Gateway Timeout` with `X-Omac-Reason: timeout`. |
| Body too large | `413 Payload Too Large`. |
| Unknown mount | `404` as above. |

---

## 14. Sandbox runtime abstraction

### 14.1 Config file

Default location: `<workdir>/.opencode/oh-my-agentic-coder.yaml`, falling back to `~/.config/omac/config.yaml`.

Shape:

```yaml
sandbox:
  default_profile: nono
  profiles:
    nono:
      command:
        - nono
        - run
        - --allow-cwd
        - --profile
        - tng-sandbox
        - --allow-file
        - "{{socket}}"
        - --env
        - "OMAC_SOCKET={{socket}}"
        - --env
        - "OMAC_SKILLS={{skills_csv}}"
        - "{{per_skill_env_flags}}"
        - --
        - "{{inner_cmd}}"
        - "{{inner_args}}"
      inner_cmd: [opencode]

    bubblewrap:
      command:
        - bwrap
        - --ro-bind
        - /
        - /
        - --dev
        - /dev
        - --proc
        - /proc
        - --bind
        - "{{socket_dir}}"
        - "{{socket_dir}}"
        - --setenv
        - OMAC_SOCKET
        - "{{socket}}"
        - --setenv
        - OMAC_SKILLS
        - "{{skills_csv}}"
        - --
        - "{{inner_cmd}}"
        - "{{inner_args}}"
      inner_cmd: [opencode]

    no-sandbox-debug:
      command: ["{{inner_cmd}}", "{{inner_args}}"]
      inner_cmd: [bash]

facade:
  idle_timeout_secs: 300
  max_body_bytes: 10485760
  base_env_passthrough: [PATH, HOME, USER, LANG, LC_ALL, LC_CTYPE, TMPDIR]
```

> Historical note: this example shows the `--env` flag for completeness with how nono profiles were originally written. Current `omac` versions inject `OMAC_*` into the runtime's process environment instead (nono propagates parent env to the inner process by default). The reference profile in `internal/config/launcher.go` is authoritative.

### 14.2 Placeholders

| Placeholder | Expansion |
| --- | --- |
| `{{socket}}` | Absolute path to `bridge.sock`. |
| `{{socket_dir}}` | Directory containing `bridge.sock`. |
| `{{inner_cmd}}` | First element of resolved inner argv (from config + CLI override). |
| `{{inner_args}}` | Remaining inner argv, token-splatted into the command array (never quoted as a single string). |
| `{{skills_csv}}` | Value of `OMAC_SKILLS`. |
| `{{per_skill_env_flags}}` | Splat of `--env OMAC_<SKILL>_BASE=…` flags, one per registered skill. Runtimes that don't use `--env` syntax must use the alternate per-skill array in a dedicated `env` section of the profile (extensible). |
| `{{workdir}}` | Absolute workdir. |

Tokens that resolve to arrays are splatted in place (tokens surrounded by other text in the same argv element are an error). This keeps argv construction explicit and shell-free.

### 14.3 Reference Nono profile

The existing `tng-sandbox.json` Nono profile remains the trust policy for filesystem/network. The `omac` Nono profile additionally:

- Adds `--allow-file {{socket}}` so the sandbox can `open(2)` the bridge socket inode (covers the Unix transport on Linux and on macOS without proxy mode).
- Adds `--read {{socket_dir}}` so component-wise path resolution during `connect(2)` succeeds without depending on Nono's `system_read_macos` group covering `$TMPDIR/omac-*`.
- Adds `--open-port {{tcp_port}}` to whitelist the facade's loopback TCP port. Per nono's [Networking](https://nono.sh/docs/cli/features/networking#localhost-ipc) docs, `--open-port` allows bidirectional `127.0.0.1:<port>` and works alongside proxy mode. **This is the transport that works under proxy mode on macOS.**
- Sets `OMAC_*` variables in its own process environment before `exec`ing nono. Nono propagates the parent environment to the inner process by default. (Nono no longer accepts literal `--env KEY=VAL` flags; the only `--env-*` flag is `--env-credential`, which is keystore-only.)

No change to the existing `tng-sandbox.json` content is required; `omac` only wraps Nono. **However, if you author a custom nono profile with `environment.allow_vars` set, you must include `OMAC_*` (or the explicit names `OMAC_SOCKET`, `OMAC_HOST`, `OMAC_PORT`, `OMAC_BASE`, `OMAC_SKILLS`, `OMAC_VERSION`, and `OMAC_<SKILL>_BASE` / `OMAC_<SKILL>_SOCKET_BASE` per registered skill) in the allow-list, or the sandbox will not see them.**

**Two transports, by design.** Per [Nono's Seatbelt documentation](https://nono.sh/docs/cli/internals/seatbelt), macOS classifies `connect(2)` on a Unix socket as `network-outbound`, not as a file operation. On Linux, AF_UNIX is governed by Landlock's file-path ACLs and is not part of its TCP port filter. Three platform-specific consequences:

1. **Linux**: `--allow-file <socket>` is sufficient for the Unix transport. AF_UNIX is purely filesystem-governed. `--open-port` is also installed; agents may use either.
2. **macOS without proxy mode**: `--allow-file` is sufficient and the default network policy is allow. Both transports work.
3. **macOS with proxy mode** (the common case — proxy mode is automatically activated whenever the active nono profile defines `custom_credentials`, sets `network_profile`, or whenever the user passes `--allow-domain`/`--credential`/`--upstream-proxy`/`--network-profile`): proxy mode installs `(deny network*)` plus an allow rule for the proxy port. The blanket deny applies to Unix-socket `connect(2)` too — `--allow-file` only grants `open(2)`, not the network-classified connect, so the Unix transport is **not reachable** in this mode. The TCP transport works because `--open-port` emits a more-specific allow rule that takes precedence.

`--block-net` is more restrictive: it installs `(deny network*)` with no `--open-port`-style escape on macOS in the current nono release. The TCP transport may or may not survive depending on rule ordering; treat as untested until verified.

The launcher config shipped with `omac` (see `internal/config/launcher.go`) provides two ready-made profiles — `nono` and `nono-netprofile` — that encode these choices. See the repository README for the full flag-to-behavior matrix.

### 14.4 CLI overrides

- **Inner harness (positional):** an optional first token after `start`/`serve`
  selects the harness — `omac start opencode` / `omac start claude` (default:
  `opencode`). It sets the default `inner_cmd` and the harness's server-launch
  and bridge conventions. Implemented by the harness registry in
  `internal/config/harness.go`.
- `--sandbox <name>` selects `sandbox.profiles.<name>`.
- `--inner <cmd>` replaces `inner_cmd[0]` (keeps remaining inner args from CLI positionals). It overrides the harness default executable.
- Positional arguments after `--` are appended to `inner_cmd` as `inner_args`.
- Example: `omac start claude --sandbox nono -- --model opus` runs Claude Code inside Nono with `--model opus`.
- Equivalent low-level form: `omac start --sandbox nono --inner claude -- --model opus`.

---

## 15. Cross-platform install scripts

### 15.1 Packaging

Each skill that ships a sidecar also ships install scripts under an `install/` directory inside the skill package:

```
.opencode/skills/slack/
├── SKILL.md                   agentskills.io discovery file (§7.1)
├── omac.yaml
├── scripts/
│   ├── slack-sidecar          sidecar entry-point (binary or script,
│   │                          referenced from omac.yaml's `command:`).
│   │                          Drop here from the install script for
│   │                          compiled languages.
│   └── …                      other agent-invokable helpers
├── src/                       (optional) source the install script compiles
└── install/
    ├── install.macos.sh
    ├── install.linux.sh
    └── install.wsl.sh         may be a symlink to install.linux.sh
```

### 15.2 Script contract

An install script MUST:

- Be idempotent (safe to re-run).
- Leave behind the binary/entrypoint that `sidecar.command` references (e.g. `scripts/slack-sidecar`). By convention this lives under `scripts/`, the agentskills.io directory for bundled executable code.
- Not require interactive input beyond `sudo` prompts.
- Exit `0` on success.

It MAY:

- Install system packages via `brew`, `apt`, `dnf`, etc. — this is the reason the user must inspect before running.
- Compile from source (`make`, `cargo build --release`, `go build`, …).

### 15.3 `register` policy

`omac register` **never** executes the script. It only prints the path and full contents. The user decides whether to run it, modify it, or replace the produced binary with their own build.

This mirrors the existing "we tell you the commands, you run them" flavour of the current Nono/Opencode setup (see `install.sh:208` and surrounding printouts).

---

## 16. Secret management (OS keychain)

Skills that talk to external services (Slack, Jira, Gmail, Gitlab, …) need credentials. Those credentials must:

- not live in the sandbox,
- not live in `omac.yaml` or any file under `.opencode/`,
- not be passed on the command line (visible in `ps`),
- not be printed to any log,
- be collectable at registration time and retrievable at start time,
- survive `deregister` + `register` cycles unless the user explicitly says otherwise.

The right storage for that is the OS keychain.

### 16.1 Declaration in `omac.yaml`

Each sidecar declares its secrets in `sidecar.secrets`:

```yaml
sidecar:
  secrets:
    - name: SLACK_BOT_TOKEN        # env-var name injected into the sidecar
      description: "Bot token (xoxb-…) for the workspace."
      required: true               # default: true
      pattern: "^xoxb-[A-Za-z0-9-]+$"   # optional validation regex
      default_from_env: SLACK_BOT_TOKEN # optional: pre-fill from shell env
      multiline: false             # default; true opens $EDITOR
```

Rules:

- `name` must match `^[A-Z_][A-Z0-9_]*$` (a valid env-var name).
- Each entry describes **one** env var that will be present in the sidecar process's environment at start time.
- `description` is required in practice (we lint it during `omac register`) so the prompt is self-explanatory.
- `pattern` is a Rust regex. Matching is non-anchored unless `^…$` are provided explicitly.
- `default_from_env` points at a host env var; if that env var is present at register time, its value is offered as a pre-filled default (still masked; the user confirms with Enter).

### 16.2 Storage backends

`omac` uses the [`keyring`](https://crates.io/crates/keyring) crate (or equivalent) to abstract over the native backend:

| OS | Backend |
| --- | --- |
| macOS | Keychain Services (`security` framework). |
| Linux (GNOME/KDE) | Secret Service API (`libsecret`, D-Bus). |
| WSL / headless Linux | Pass-through to the native backend if present; otherwise fallback to an age-encrypted file under `~/.local/share/omac/secrets.age` with the passphrase unlocked once per session via keyring if available. |
| Windows (native, future) | Windows Credential Manager. |

The backend selection is logged once on first use (`omac doctor` shows the active backend). Users can pin a backend via `OMAC_KEYRING_BACKEND=secret-service|keychain|file`.

### 16.3 Naming convention

Each secret is one keychain entry:

- `service`: `omac/<skill-name>`
- `account`: `<secret.name>` (e.g. `SLACK_BOT_TOKEN`)
- `label` (where the backend supports it): `omac · <skill> · <secret.name>`

This gives users a clean view in `Keychain Access.app` / `seahorse` and lets them manually revoke any single credential without touching `omac`.

The skill name used for scoping is the **registered** name (what is in `sidecar.json`), not the folder name, so cloned skills with different registrations keep their credentials separate.

### 16.4 Prompting flow at `omac register`

For each declared secret, in order:

1. Look up the keychain entry. If present and `--reprompt-secrets` was not passed, skip.
2. If `default_from_env` is set and the corresponding host env var is present, show:
   `SLACK_BOT_TOKEN (from $SLACK_BOT_TOKEN, hidden): [press Enter to accept]`
3. Otherwise prompt:
   `SLACK_BOT_TOKEN: ` with no echo. On terminals without no-echo support, print a one-line warning and fall back to echoed input (the user can still paste from a password manager).
4. Validate against `pattern`. On mismatch, re-prompt up to 3 times, then abort with exit 9.
5. Store via keyring. A successful set prints `  stored SLACK_BOT_TOKEN` (name only, never value).
6. If the entry is `required: false` and the user enters an empty line, the entry is skipped (any prior keychain value is left untouched).

Non-interactive alternatives:

- `--secrets-from <file>`: `KEY=VALUE` lines; unknown keys are an error, missing required keys are an error, validated against `pattern`. The file is not deleted by `omac` (that is the user's choice) but is never read from again.
- `OMAC_SECRET_<NAME>` environment variables: for CI. If set at `register` time and not already in the keychain, stored as if prompted. `omac start --use-env-secrets` allows bypassing the keychain entirely and taking the same env vars directly at start time.

### 16.5 Retrieval at `omac start`

- One keychain call per declared secret, per skill.
- Cached in the `omac` process for the remainder of the run (not persisted).
- Required-but-missing secrets abort `start` with exit 9 and a precise instruction:
  `SLACK_BOT_TOKEN missing. Run: omac secrets set slack SLACK_BOT_TOKEN`
- Injected into the sidecar's env (§12, step 5c) and nowhere else. Never injected into the sandbox.

### 16.6 In-process handling

- Secrets are wrapped in a `Secret<String>` / `SecretVec<u8>` type (via the `secrecy` crate) that blocks `Debug`/`Display` and zeroizes on drop.
- Buffers used for prompting are zeroized after copying into the secret wrapper.
- `ps(1)` never sees a secret because argv is unchanged.
- `/proc/<pid>/environ` is readable only by the user the sidecar runs as; that is considered acceptable under the trust model (§16.9). Sidecars that are especially sensitive may opt out via `sidecar.secrets_via: "stdin"` (future; §20) to receive secrets on stdin rather than in env.

### 16.7 Removal and rotation

- `omac secrets unset <skill> <name>` — deletes a single entry.
- `omac secrets set <skill> <name>` — overwrite with a fresh prompt.
- `omac deregister <skill>` — keeps keychain entries by default; `--purge-secrets` deletes all `omac/<skill>/*` entries.
- Rotation is just: `omac secrets set <skill> <NAME>` followed by an `omac start` restart.
- Skills whose upstream supports it should expose a sidecar route like `POST /_admin/reload-secrets` that rereads env-injected values; this is a sidecar-level concern, not an `omac` feature.

### 16.8 Trust boundary (revisited)

- **Inside sandbox**: untrusted with respect to the host. It may run agent-generated code that tries to exfiltrate anything it sees. Therefore the sandbox sees only the socket; the sandbox never sees any secret, any host env var that isn't explicitly forwarded, or any part of the keychain.
- **Facade (`omac`)**: semi-trusted. Written and distributed by us. It handles secrets only long enough to inject them into a child process and then drops them.
- **Sidecar**: trusted with its own secrets (they appear in its env). Trusted with respect to the upstream it talks to. The facade enforces body-size and timeout limits to blunt DoS from inside the sandbox.
- **Keychain**: trusted; protected by the OS login session / screensaver lock.
- **Host**: fully trusted.

### 16.9 Socket permissions

- `bridge.sock` is created with `0600`, owned by the invoking user.
- The containing directory is `0700`.
- The socket path contains a workdir hash to avoid cross-workdir collision.

### 16.10 `env_passthrough`

- Strict allowlist. Unknown variables are dropped.
- Never use `env_passthrough` for secrets — that leaks them to anyone who can read your shell config. Use `sidecar.secrets` instead.
- A small base list (`PATH`, `HOME`, `USER`, `LANG`, `LC_*`, `TMPDIR`) is always forwarded so normal programs (brew-installed tools, python interpreters) work. Configurable via `facade.base_env_passthrough`.

### 16.11 Threats out of scope

- Malicious skills (distribution-layer problem; marketplace signing is future work).
- Kernel-level sandbox escape (that's Nono's job).
- A root attacker on the host (they own the keychain anyway).
- Side-channel leaks between sidecars (they share the host user).

---

## 17. Observability

### 17.1 Facade access log

One line per request, newline-delimited JSON, to `logs/facade.log`:

```json
{"ts":"2026-04-22T08:30:11.031Z","method":"POST","mount":"slack","path":"/api/chat.postMessage","upstream_status":200,"bytes_in":412,"bytes_out":187,"duration_ms":73}
```

### 17.2 Sidecar logs

- `logs/<skill>.log` — raw stdout+stderr of the sidecar, line-buffered.
- Rotated on start (previous run archived to `<skill>.log.1`).

### 17.3 Health surfacing

`omac list` shows per-skill `HEALTH` column: `ok` (last probe 2xx), `degraded` (last probe failed), `unknown` (facade not running).

### 17.4 Verbose mode

`omac start --verbose` adds structured logs on lifecycle events (spawn, probe, route-mount, upgrade, shutdown).

---

## 18. Failure modes & recovery

| Failure | Detection | Response |
| --- | --- | --- |
| Port allocation collision | `bind(:0)` race on reuse | Retry up to 3 times, then fail start. |
| Sidecar fails initial health | Health-probe loop times out | Kill started sidecars, remove runtime dir, exit `6`. |
| Sidecar crashes after start | `waitpid` or TCP refused on a live request | Restart with exponential backoff, max 5 retries, then mark degraded. |
| Stale `bridge.sock` from a previous crash | `connect()` fails on start | `unlink` and recreate. |
| Orphaned sidecars from a previous crash | `pids/<skill>.pid` file + `kill(0, pid)` check + cmdline match | `omac doctor --fix` kills them. |
| `sidecar.json` concurrent write | `flock` contention | Retry briefly; fail after 5 s with a clear message. |
| `omac.yaml` changed since registration | `bundle_hash` mismatch | Refuse to start; diff printed; `--accept-skill-changes` opts in. |

---

## 19. Compatibility & migration

### 19.1 `himalaya-email`

Today: user runs `uv run .opencode/skills/himalaya-email/scripts/gpg-service.py` by hand; skill points at `http://127.0.0.1:7823`.

Migration:

1. Add a `sidecar` block to `omac.yaml` (§7.3), including any secrets the GPG service needs (e.g. IMAP/SMTP passwords) as `sidecar.secrets`.
2. Update `SKILL.md` to read `OMAC_HIMALAYA_EMAIL_BASE` instead of the hard-coded URL, with a fallback to the old URL so users who haven't adopted `omac` are not broken.
3. Users run `omac register himalaya-email` once (enter any prompted credentials → stored in keychain), run the install script, then `omac start`.

### 19.2 Skills without sidecars

Continue to work unchanged. `omac` ignores them in `register` (exit 2) and does not touch them at start.

### 19.3 Running without `omac`

`tng-opencode` continues to work as today. Skills that declare a sidecar but whose users never run `omac` will see `OMAC_*` env missing and are expected to either fail cleanly or fall back to a user-started service.

---

## 20. Open questions / future work

- **Peer-credential auth on the socket** (via `SO_PEERCRED` / `LOCAL_PEERCRED`) to bind routes to a single known pid/uid.
- **Multi-workdir shared sidecar** (e.g. one Slack sidecar serving several sandboxes). Would require sidecar-side multi-tenancy and shifts the trust model.
- **Named-pipe support on native Windows** (non-WSL). Likely a new transport module; the rest of the design is unchanged.
- **Signed skill metadata** — marketplace emits a signature over `meta.yaml` (and, if shipped, `omac.yaml`) plus install scripts; `omac register` verifies before printing.
- **Per-skill egress policy** — sidecars themselves could run under a secondary Nono profile to constrain what they reach outbound.
- **Access-log redaction** (`sidecar.log_redact` glob list of header names and query parameters).
- **Per-skill resource quotas** (cpu, mem, max concurrent requests) via cgroups on Linux.
- **Structured route descriptors** — `sidecar.routes: [{method, path, description}]` for auto-generated inline-sandbox documentation.
- **`sidecar.secrets_via: "stdin" | "env"`** — for sidecars that must not receive secrets via `/proc/<pid>/environ`; `omac` would pipe a JSON blob of secrets into the child on stdin instead.
- **Keychain group handoff** — single `omac-session` master key unlocked once per login, with per-skill secrets wrapped under it, for backends without per-item ACL.

---

## Appendix A — Complete `omac.yaml` example (slack)

```yaml
name: slack
type: skill
version: 1.2.0
description: Slack REST bridge for agent-coding sandboxes.
author: tngtech
dependencies: []

sidecar:
  command: ["./scripts/slack-sidecar", "--port", "${SIDECAR_PORT}"]
  mount: slack
  env_passthrough:
    - HTTPS_PROXY
  secrets:
    - name: SLACK_BOT_TOKEN
      description: "Bot token (xoxb-…) for the Slack workspace."
      required: true
      pattern: "^xoxb-[A-Za-z0-9-]+$"
    - name: SLACK_APP_TOKEN
      description: "App-level token (xapp-…) for Socket Mode."
      required: false
      pattern: "^xapp-[A-Za-z0-9-]+$"
  health:
    path: /status
    initial_delay_ms: 200
    timeout_ms: 5000
    interval_ms: 500
  install_scripts:
    macos: install/install.macos.sh
    linux: install/install.linux.sh
    wsl:   install/install.linux.sh
  protocols: ["http", "sse"]
  limits:
    max_body_bytes: 10485760
    idle_timeout_secs: 300
```

## Appendix B — Complete `.opencode/sidecar.json` example

```json
{
  "version": 1,
  "registered": [
    {
      "name": "slack",
      "skill_dir": ".opencode/skills/slack",
      "bundle_hash": "sha256:9f4b3a…",
      "registered_at": "2026-04-22T08:15:00Z"
    },
    {
      "name": "himalaya-email",
      "skill_dir": ".opencode/skills/himalaya-email",
      "bundle_hash": "sha256:1c77e2…",
      "registered_at": "2026-04-22T08:20:12Z"
    }
  ]
}
```

## Appendix C — Reference Nono invocation produced by the launcher

With two skills (`slack`, `himalaya-email`) registered and profile
`nono` (the facade allocates an ephemeral TCP port, e.g. `41017`, and
binds both transports):

```
# omac sets these in its own process env before exec'ing nono;
# nono propagates the parent env to the inner process.
OMAC_BASE=http://127.0.0.1:41017/
OMAC_HOST=127.0.0.1
OMAC_PORT=41017
OMAC_SOCKET=/tmp/omac-9f4b3a8c2e10/bridge.sock
OMAC_SKILLS=slack,himalaya-email
OMAC_SLACK_BASE=http://127.0.0.1:41017/slack
OMAC_SLACK_SOCKET_BASE=http+unix://%2Ftmp%2Fomac-9f4b3a8c2e10%2Fbridge.sock/slack
OMAC_HIMALAYA_EMAIL_BASE=http://127.0.0.1:41017/himalaya-email
OMAC_HIMALAYA_EMAIL_SOCKET_BASE=http+unix://%2Ftmp%2Fomac-9f4b3a8c2e10%2Fbridge.sock/himalaya-email
OMAC_VERSION=0.1.0

nono run \
  --allow-cwd \
  --profile tng-sandbox \
  --allow-file /tmp/omac-9f4b3a8c2e10/bridge.sock \
  --read       /tmp/omac-9f4b3a8c2e10 \
  --open-port  41017 \
  -- \
  opencode
```

Three non-obvious things in that argv:

1. **`--open-port <tcp-port>`** is the documented way to allow
   bidirectional `127.0.0.1:<port>` from inside a nono sandbox (see
   the [Localhost IPC](https://nono.sh/docs/cli/features/networking#localhost-ipc)
   section of nono's networking docs). It works alongside proxy mode
   (auto-activated by `tng-sandbox.json`'s `custom_credentials.tng_skills`
   block) — without this flag, Seatbelt's `(deny network*)` would
   block the agent from reaching either transport.

2. **`--allow-file` + `--read` cover the Unix transport** for
   non-proxy-mode setups (Linux Landlock + macOS without proxy mode).
   They are harmless under proxy mode (where the AF_UNIX `connect(2)`
   is blocked anyway).

3. **Env-var injection is via process env, not flags.** Nono no
   longer accepts a literal `--env KEY=VAL` flag (the only `--env-*`
   flag is `--env-credential`, which is keystore-only). `omac` sets
   `OMAC_*` in nono's process environment before exec; nono
   propagates the parent env to the inner process by default.
   Profiles with `environment.allow_vars` set must include `OMAC_*`
   in the list. The shipped `tng-sandbox.json` leaves that section
   unset, so the default-allow behaviour delivers them automatically.

## Appendix D — End-to-end walkthrough

```bash
# 1. Install omac (separate from Nono, from skill-installer).
brew install tng/tap/oh-my-agentic-coder

# 2. Install a skill from the marketplace (existing workflow).
scripts/install.sh slack

# 3. Register its sidecar with omac in this workdir.
omac register slack
# → prompts for SLACK_BOT_TOKEN (masked) and optionally SLACK_APP_TOKEN,
#   stores them in the macOS Keychain under service "omac/slack",
#   then prints the full macOS install script and tells you to run it.

# 4. Inspect and run the install script manually.
less .opencode/skills/slack/install/install.macos.sh
bash .opencode/skills/slack/install/install.macos.sh
# → produces .opencode/skills/slack/scripts/slack-sidecar

# 5. (Optional) inspect what is stored and verify health.
omac secrets list slack
omac doctor

# 6. Start the whole stack.
omac start
# → reads secrets from the keychain, spawns sidecars with them in env,
#   opens bridge.sock, execs Nono + OpenCode. The sandbox sees the socket
#   but not the tokens.

# 7. Inside OpenCode, the skill can now call:
curl --unix-socket "$OMAC_SOCKET" http://x/slack/api/chat.postMessage ...

# 8. When OpenCode exits, omac tears everything down and zeroes in-memory secrets.

# 9. Later: rotate a token without re-registering.
omac secrets set slack SLACK_BOT_TOKEN
omac start
```

---

*End of design document.*
