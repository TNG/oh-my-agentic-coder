# Creating a Skill for `omac`

This guide explains how to author a skill that plugs into the
`oh-my-agentic-coder` (`omac`) execution-shell. Skills are the unit of
extension: each one ships a small **sidecar** HTTP service that runs **outside**
the agent sandbox and is reached **inside** the sandbox through `omac`'s
single Unix-domain-socket facade.

If you have not already, read:

- [`README.md`](./README.md) — high-level workflow and CLI reference.
- [`oh-my-agentic-coder.md`](./oh-my-agentic-coder.md) — full design doc.
- [`.opencode/skills/echo-rest/`](./.opencode/skills/echo-rest/) — the
  reference skill. Copy it as a starting point.

---

## 1. Why a sidecar?

The agent runs in a sandbox (`nono` by default) that may have restricted
network access and never sees your secrets. Each skill provides a sidecar
that:

- Holds the credentials (injected by `omac` from the OS keychain at start time).
- Talks to the real out-of-sandbox service (Slack API, IMAP server, GitHub, …).
- Exposes a small HTTP API on `127.0.0.1:$SIDECAR_PORT`.

`omac` proxies sandbox-side requests on a Unix socket to the right sidecar
based on the URL prefix (the `mount`). The sandbox never gets the secret;
it only gets a socket path.

```
   inside sandbox                       outside sandbox (host)
   ───────────────                      ───────────────────────
   curl --unix-socket $OMAC_SOCKET \
        http://x/<mount>/...   ─────►   omac facade  ─────►  sidecar
                                                            (has secrets,
                                                             talks to real
                                                             upstream API)
```

---

## 2. Skill on-disk layout

A skill lives under `.opencode/skills/<name>/` in the workdir:

```
.opencode/skills/<name>/
├── meta.yaml                 required — schema below
├── <your sidecar>            any executable: python, node, go binary, bash, …
└── install/
    ├── install.macos.sh      optional but recommended
    └── install.linux.sh      optional but recommended
```

Naming rules:

- `name` must match the directory name.
- `mount` (URL prefix) must match `^[a-z0-9][a-z0-9-]*$`.
- Secret env-var names must match `^[A-Z_][A-Z0-9_]*$`.

---

## 3. `meta.yaml` schema

Minimal example (based on `echo-rest`):

```yaml
name: my-skill                    # required; must equal the directory name
type: skill                       # informational
version: 0.1.0
description: One-line summary shown in `omac list`.
author: you
dependencies: []

sidecar:
  # argv to spawn the sidecar process. ${SIDECAR_PORT} is substituted by omac.
  # Prefer reading $SIDECAR_PORT from env; argv is visible to all users via `ps`.
  command: ["python3", "sidecar.py"]

  # URL prefix on the bridge socket. Inside the sandbox the skill is reached at
  #   curl --unix-socket "$OMAC_SOCKET" http://x/<mount>/...
  # Optional; defaults to the skill name.
  mount: my-skill

  # Optional: pass through host env vars verbatim. Use sparingly — values
  # leak from the invoking user's shell, not the OS keychain.
  env_passthrough:
    - SOMETHING_NONSECRET

  # Declared secrets. `omac register` prompts (masked input), stores in the
  # OS keychain under service "omac/<skill>", and injects at sidecar start.
  secrets:
    - name: MY_API_TOKEN
      description: "Token for the upstream API."
      required: true               # default is true
      pattern: "^[A-Za-z0-9_-]{16,}$"   # optional regex validation
      default_from_env: MY_API_TOKEN     # optional: pre-fill from env at prompt
      multiline: false                   # optional: PEM keys etc.

  # Liveness probe. omac waits for `path` to return 2xx before mounting routes.
  health:
    path: /status                  # default /status
    initial_delay_ms: 100          # default 200
    timeout_ms: 4000               # default 5000
    interval_ms: 200               # default 500

  # Per-OS install scripts. omac PRINTS these at register time; it never runs them.
  # Use them to document `brew install …` / `apt install …` / build steps.
  install_scripts:
    macos: install/install.macos.sh
    linux: install/install.linux.sh

  # Currently only "http" is meaningful.
  protocols: ["http"]

  # Optional: per-skill proxy limits.
  limits:
    max_body_bytes: 10485760       # 10 MiB
    idle_timeout_secs: 300
```

See `internal/config/meta.go` for the authoritative struct definitions.

---

## 4. The sidecar HTTP server

Your sidecar is a normal HTTP server. Requirements:

1. **Bind on `127.0.0.1` only**, never `0.0.0.0`.
2. **Read the port from the env var `SIDECAR_PORT`** (set by `omac`). Do
   not take it from argv — argv is world-readable via `ps`.
3. **Implement the health route** declared in `health.path` (default
   `/status`). Return any 2xx body; e.g. `{"ok":true}`.
4. **Read secrets from environment variables** named in `secrets` /
   `env_passthrough`. They are injected into the sidecar process only.
5. **Never echo secrets back** in responses or logs. Echoing a fingerprint
   (e.g. `sha256(secret)[:12]`) is fine for debugging.

`omac` also injects:

| Env var          | Meaning                                                |
| ---------------- | ------------------------------------------------------ |
| `SIDECAR_PORT`   | TCP port the sidecar must bind on (loopback only).     |
| `SIDECAR_SKILL`  | The skill name (handy for log lines).                  |
| `OMAC_WORKDIR`   | Absolute path of the workdir omac was invoked in.      |
| Each declared secret | The value from the keychain (or `env_passthrough` host env). |

### Minimal Python sidecar

```python
#!/usr/bin/env python3
import json, os, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SKILL  = os.environ.get("SIDECAR_SKILL", "my-skill")
PORT   = int(os.environ.get("SIDECAR_PORT", "0"))
TOKEN  = os.environ.get("MY_API_TOKEN", "")

class H(BaseHTTPRequestHandler):
    def _json(self, code, body):
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path == "/status":
            return self._json(200, {"ok": True, "skill": SKILL})
        # … your real routes here, calling the upstream API with TOKEN.
        self._json(404, {"error": "not found"})

if PORT == 0:
    print("SIDECAR_PORT not set", file=sys.stderr); sys.exit(2)
ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
```

### Streaming (Server-Sent Events, long polls, WebSockets)

The facade is a streaming reverse proxy. SSE works out of the box: set
`Content-Type: text/event-stream`, do not set `Content-Length`, and call
`flush()` after each frame. The facade adds `X-Accel-Buffering: no`
automatically. See `echo-rest`'s `/tick` route for a worked example.

---

## 5. Install scripts

`omac register <skill>` prints `install/install.<os>.sh` to stdout but
**never executes it**. Keep these scripts:

- Idempotent.
- Free of side effects on `$HOME` or globally installed packages where possible.
- Self-contained: install language runtimes, fetch binaries, compile, etc.

Example `install/install.macos.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail

if ! command -v python3 >/dev/null; then
  echo "python3 is required: brew install python" >&2
  exit 1
fi

# Optional: create a venv and install requirements
# python3 -m venv .venv
# .venv/bin/pip install -r requirements.txt
echo "my-skill: ready."
```

Mark them executable: `chmod +x install/install.*.sh`.

---

## 6. Routes the agent will see (URL rewriting)

Inside the sandbox the agent calls:

```
curl --unix-socket "$OMAC_SOCKET" http://x/<mount>/<your-route>
```

The facade strips the `/<mount>` prefix and forwards `/<your-route>` to
your sidecar, with header `X-Forwarded-Prefix: /<mount>`. So a sidecar
that implements `GET /status` is reachable from the sandbox as
`GET http://x/<mount>/status`.

`omac start` also exports a set of env vars into the sandbox so agents
can discover the facade without having to know its hash-derived path:

| Env var | Value | Notes |
| --- | --- | --- |
| `OMAC_SOCKET` | `/tmp/omac-<hash>/bridge.sock` | Unix-domain socket path. |
| `OMAC_HOST` | `127.0.0.1` | Loopback host the facade is bound to. |
| `OMAC_PORT` | e.g. `41017` | Ephemeral TCP port. |
| `OMAC_BASE` | `http://127.0.0.1:<port>/` | Facade root, TCP form. |
| `OMAC_SKILLS` | e.g. `echo,slack,himalaya-email` | Comma-separated mount names. Empty when no skills are registered. |
| `OMAC_VERSION` | e.g. `0.1.5` | The omac binary's version string. |
| `OMAC_<MOUNT>_BASE` | `http://127.0.0.1:<port>/<mount>/` | **TCP** URL for this skill. Prefer this one — it works under nono proxy mode on macOS, where Seatbelt's `(deny network*)` blocks Unix-socket `connect(2)`. |
| `OMAC_<MOUNT>_SOCKET_BASE` | `http+unix://%2Ftmp%2Fomac-<hash>%2Fbridge.sock/<mount>/` | Unix-socket form. Lower overhead when available; fails on macOS under nono proxy mode. |

The `<MOUNT>` portion of the per-skill var names is derived from the
mount string by uppercasing letters and replacing every non-alphanumeric
character with `_`. Examples (pinned in
`internal/sandbox/launcher_test.go`):

| `mount:` in meta.yaml | Env var name |
| --- | --- |
| `echo` | `OMAC_ECHO_BASE` |
| `himalaya-email` | `OMAC_HIMALAYA_EMAIL_BASE` |
| `mail2` | `OMAC_MAIL2_BASE` |
| `a-b_c` | `OMAC_A_B_C_BASE` |

Both transports are advertised at the same time so a sandbox-aware
client library can fall back from one to the other. As a rule of thumb
inside the sandbox, **use the TCP form first**.

---

## 7. Develop & test the skill

### 7.1 Validate the metadata

```bash
omac register --no-secrets my-skill   # validates meta.yaml + adds to registry
omac doctor                           # runs sanity checks
omac list                             # shows mount, secret count, binary status
```

### 7.2 Run the stack outside the sandbox

The fastest dev loop is `--no-sandbox`, which spawns sidecars + the facade
but execs your inner command directly instead of `nono run …`:

```bash
omac start --no-sandbox --inner bash
# inside the spawned shell:
echo "$OMAC_SOCKET"
curl --unix-socket "$OMAC_SOCKET" http://x/my-skill/status
# or via the TCP form (works the same outside the sandbox):
curl "$OMAC_MY_SKILL_BASE/status"
```

`omac start` runs successfully even with no skills registered yet — it
will just bring up the facade with no upstream routes and exec the
inner command. Useful when you're iterating on a sidecar before the
first `omac register` of the day.

### 7.3 Run the stack inside `nono`

```bash
omac start                    # default profile: nono
omac start --sandbox nono-netprofile   # network-restricted variant
```

### 7.4 Inspect logs on failure

If a request returns `503 X-Omac-Reason: sidecar-down`, look at:

```
$TMPDIR/omac-<workdir-hash>/logs/<skill>.log
```

That file captures the sidecar's stderr/stdout.

### 7.5 Use a smoke-test client

Model your manual smoke test on `demo-client.sh` in the repo root. It
hits every route on the echo-rest reference skill via both the TCP and
Unix-socket transports, and prints the values of every `OMAC_*` env var
it received. Run it as the inner command:

```bash
omac start --no-sandbox --inner=./demo-client.sh
```

(Anything after `--` becomes argv for the inner command, so you can pass
flags through if your client takes any.)

---

## 8. Secrets handling — best practices

- **Always declare** real credentials under `sidecar.secrets`. They go
  through the OS keychain (`omac/<skill>/<NAME>` service URI).
- **Reserve `env_passthrough`** for non-secret config or for CI runners
  where the keychain is unavailable. Document this in your description.
- **Validate with `pattern`** so users get an early failure at register
  time rather than a 500 from the upstream API.
- **Never log secrets**. Use a fingerprint (`sha256(secret)[:12]`) to
  prove injection in `/whoami`-style debug routes.
- **Rotate without re-registering** with `omac secrets set <skill> <NAME>`.

---

## 9. Distributing the skill

`omac` is the **runtime**, not an installer. The marketplace installer
(e.g. `scripts/install.sh <skill>`) drops your skill directory under
`.opencode/skills/<name>/`. To distribute:

1. Pick a stable `name` (kebab-case) and `mount` (URL-safe).
2. Pin a `version` and bump it on breaking changes.
3. Ship the source layout described in §2.
4. Make sure `install/install.<os>.sh` documents every host-side
   dependency (interpreters, build tools, native libs).
5. Test with at least:
   - `omac register --no-secrets <skill>`
   - `omac start --no-sandbox --inner bash` and a smoke-test script.
   - `omac start` (full sandbox path) on macOS and Linux.

Users will then:

```bash
scripts/install.sh <your-skill>     # marketplace pulls your tree
omac register <your-skill>          # prompts for declared secrets
bash .opencode/skills/<your-skill>/install/install.macos.sh
omac start
```

---

## 10. Checklist before shipping

- [ ] `meta.yaml` validates (`omac register --no-secrets …` succeeds).
- [ ] `mount` matches `^[a-z0-9][a-z0-9-]*$`.
- [ ] Sidecar binds on `127.0.0.1:$SIDECAR_PORT` (not `0.0.0.0`).
- [ ] `GET /status` returns 2xx within `health.timeout_ms`.
- [ ] All credentials declared under `secrets:` (not just `env_passthrough`).
- [ ] No secret value logged or returned in any response body.
- [ ] `install/install.macos.sh` and `install/install.linux.sh` are
      executable, idempotent, and exit non-zero on missing prerequisites.
- [ ] Streaming endpoints (if any) use `Content-Type: text/event-stream`,
      no `Content-Length`, and call `flush()` per frame.
- [ ] Smoke-tested end-to-end with `omac start` (sandboxed) and the agent
      reaching the sidecar via `$OMAC_SOCKET`.

---

## References inside this repo

- Reference skill: [`.opencode/skills/echo-rest/`](./.opencode/skills/echo-rest/)
- `meta.yaml` schema: [`internal/config/meta.go`](./internal/config/meta.go)
- Sidecar lifecycle (env injection, port assignment): [`internal/supervisor/supervisor.go`](./internal/supervisor/supervisor.go)
- Facade routing & SSE: [`internal/facade/facade.go`](./internal/facade/facade.go)
- Sandbox launcher (`OMAC_*_BASE` env splat): [`internal/sandbox/launcher.go`](./internal/sandbox/launcher.go)
- Smoke-test client: [`demo-client.sh`](./demo-client.sh)
