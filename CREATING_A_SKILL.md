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

A skill is a directory containing a `meta.yaml`. omac looks in two
locations, in this order:

1. **Workdir-local** — `<workdir>/.opencode/skills/<name>/`. The
   project-specific layer; what most users mean by "their skills".
2. **User-global** — `$XDG_CONFIG_HOME/opencode/skills/<name>/`
   (default `~/.config/opencode/skills/<name>/`). Skills that you want
   available across every project on your machine. As a legacy
   fallback, `~/.opencode/skills/<name>/` is also consulted.

If a skill name exists in both layers, the **workdir copy wins** (so
a project can fork a user-global skill without touching the global
one). Registration data — `sidecar.json`, `skill-config.yaml`, and OS
keychain entries — always lives in the workdir, regardless of which
layer the source came from. Each project explicitly opts into a
user-global skill by running `omac register <name>` in that project's
workdir.

Inside either location the per-skill layout is the same:

```
<location>/skills/<name>/
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

  # Declared non-secret config fields. `omac register` prompts (echoing input),
  # stores in plain YAML under <workdir>/.opencode/skill-config.yaml, and
  # injects at sidecar start as env vars exactly the same way as secrets.
  # See §8 below for the full type system.
  config:
    - name: API_BASE_URL
      type: string                          # default: string
      description: "Base URL of the upstream API."
      default: "https://api.example.com"
      pattern: "^https://"
    - name: ENABLE_DEBUG
      type: bool
      default: "false"
      required: false
    - name: REQUEST_TIMEOUT_MS
      type: int
      default: "5000"
    - name: REGION
      type: enum
      choices: [eu-central-1, us-east-1, ap-northeast-1]
      default: eu-central-1

  # Liveness probe. omac waits for `path` to return 2xx before mounting routes.
  health:
    path: /status                  # default /status
    initial_delay_ms: 100          # default 200
    timeout_ms: 4000               # default 5000
    interval_ms: 200               # default 500

  # Per-OS install scripts. omac SURFACES these at register time
  # (path + run-it-yourself hint); it never executes them, and as of
  # the current omac it no longer dumps their contents to the terminal.
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
| Each declared config field | The value from `.opencode/skill-config.yaml` (canonicalized — see §8). |

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

`omac register <skill>` surfaces the path of `install/install.<os>.sh`
and reminds the user to run it manually (it does NOT print the script
body, and never executes it). Keep these scripts:

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
| `OMAC_<MOUNT>_BASE` | `http://127.0.0.1:<port>/<mount>` | **TCP** URL for this skill, without a trailing slash. Prefer this one — it works under nono proxy mode on macOS, where Seatbelt's `(deny network*)` blocks Unix-socket `connect(2)`. |
| `OMAC_<MOUNT>_SOCKET_BASE` | `http+unix://%2Ftmp%2Fomac-<hash>%2Fbridge.sock/<mount>` | Unix-socket form, without a trailing slash. Lower overhead when available; fails on macOS under nono proxy mode. |

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
omac config show my-skill             # inspect resolved config + secret fingerprints
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

### 7.4 What `omac start` verifies before running

`omac start` reconciles the on-disk state against what was registered
and refuses to spawn anything if the gap is wider than the user has
explicitly accepted. Four classes of drift, in the order they're
checked:

1. **Skill directory deleted** (registered skill no longer on disk).
   Auto-deregistered silently, with a one-line `[info]` log message
   and a hint about how to purge the leftover secrets/config:
   ```
   [info] my-skill: skill directory missing on disk; auto-deregistered.
   Stored secrets and config remain. To purge: omac deregister
   --purge-secrets --purge-fields my-skill
   ```
   `start` continues with whatever skills remain. The kept secrets +
   config are insurance against an accidental `rm -rf` on the skills
   tree.

2. **Unregistered skill on disk** (a directory with `meta.yaml` under
   *either* skill source — the workdir-local layer or the user-global
   layer, see §2 — that omac has never seen). Refuses to start;
   prints the exact register command for each one. Re-registration is
   mandatory because it's the only point at which secret-prompting and
   config-prompting happen.

3. **Bundle hash drift** (the registered skill's source files changed
   since register). The bundle hash covers `meta.yaml` and every
   sidecar source file (helper modules, install scripts) — but
   excludes runtime artifacts like `__pycache__/`, `.venv/`,
   `node_modules/`, `.git/`, `.DS_Store`, `*.pyc`, editor swap files,
   so a `pip install` doesn't trip detection. Refuses unless
   `--accept-skill-changes` is passed (or you re-register with
   `omac register --force my-skill`). The flag exists for the case
   where you've intentionally edited the skill in place during
   development and don't want to re-prompt.

4. **Missing required config field** (a `config:` entry with
   `required: true` that has no stored value, no `default:`, and no
   resolvable `default_from_env:`). Refuses with the list of missing
   names and the exact register command:
   ```
   omac start: my-skill: required config field(s) missing: API_BASE_URL, REGION
   Run: omac register --reprompt-fields my-skill
   ```

In all refusal cases the exit is non-zero so wrapper scripts can
catch the condition and prompt the user.

### 7.5 Inspect logs on failure

If a request returns `503 X-Omac-Reason: sidecar-down`, look at:

```
$TMPDIR/omac-<workdir-hash>/logs/<skill>.log
```

That file captures the sidecar's stderr/stdout.

### 7.6 Use a smoke-test client

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
- **Use `sidecar.config`** for non-secret operational values (URLs,
  flags, region names) instead of overloading `secrets:` or
  `env_passthrough:`. See §9 for the typed-field schema.
- **Reserve `env_passthrough`** for the legacy fallback case: a value
  that's normally a secret, but in some environments (CI runners
  without a keychain) needs to come from the host shell. Document this
  in your description.
- **Validate with `pattern`** so users get an early failure at register
  time rather than a 500 from the upstream API.
- **Never log secrets**. Use a fingerprint (`sha256(secret)[:12]`) to
  prove injection in `/whoami`-style debug routes.
- **Rotate without re-registering** with `omac secrets set <skill> <NAME>`.

---

## 9. Configuration fields (non-secret config)

Skills often have *operational* configuration that isn't a credential
but still varies between users or deployments: an API base URL, a
default region, a feature flag, a retry limit. Putting these in the
keychain is overkill (and obscures them); hard-coding them in
`meta.yaml` makes the skill un-reusable. Declare them under
`sidecar.config:` instead.

The lifecycle:

1. `omac register` prompts for every declared field (echoing input
   visibly, **not** masked — these are not secret).
2. Values are stored in plain YAML under
   `<workdir>/.opencode/skill-config.yaml`, mode `0600`.
3. At `omac start` time the values are injected into the sidecar's
   environment alongside secrets, so the sidecar reads them with
   `os.environ.get("FIELD_NAME")` exactly like a secret.

### 9.1 Field schema

```yaml
sidecar:
  config:
    - name: FIELD_NAME              # required; ^[A-Z_][A-Z0-9_]*$ (env-var name)
      description: "Shown above the prompt."
      type: string                  # one of: string (default) | bool | int | enum
      required: true                # default true; false ⇒ stored only if user
                                    #   types something
      default: "..."                # pre-filled at the prompt; press Enter to accept
      default_from_env: "..."       # if `default` is empty and this env var is set
                                    #   in the host shell at register time, use it
                                    #   as the default
      pattern: "^https://"          # string-only; regex applied to input
      choices: [a, b, c]            # enum-only; non-empty list
```

### 9.2 Type system

| `type:` | Accepted input | Stored as |
|---|---|---|
| `string` (default) | any text; if `pattern` set, must match | input verbatim |
| `bool` | `true / false / yes / no / y / n / 1 / 0 / on / off` (case-insensitive) | the canonical `"true"` or `"false"` |
| `int` | base-10 64-bit integer (whitespace tolerated) | `strconv.FormatInt` rendering |
| `enum` | exact match against one of `choices` | input verbatim |

`pattern` is only valid with `type: string`; `choices` is required for
`type: enum` and forbidden for the others. Defaults are validated at
meta-load time using the same rules as input — an `int` default of
`"twelve"` is a meta error, not a runtime surprise.

### 9.3 Naming, collisions, and `env_passthrough`

Field names share the env-var namespace with `secrets:` and
`env_passthrough:`. The validation rules:

- A field name colliding with a `secrets:` entry is rejected at
  meta-load time. Pick one or the other; secrets are stricter.
- A field name colliding with an `env_passthrough:` entry is rejected.
  `env_passthrough` is for non-declared host env you want to opt
  into; if the value is important enough to declare, declare it under
  `config:` (or `secrets:`) instead.
- A `secrets:` entry colliding with `env_passthrough` is **allowed**.
  This is the established fallback pattern: declare the secret, but
  also list it in `env_passthrough` so it works in environments where
  the keychain is unavailable (sandboxed CI runners). At runtime the
  keychain value wins over the host-env value when both are present.

### 9.4 Updating values after registration

Two ways:

```bash
omac register --reprompt-fields my-skill   # interactive, keeps secrets
```

…or edit `.opencode/skill-config.yaml` directly (mode 0600 — chmod it
back if you `chown` it). Re-running `omac start` picks up the new
values on the next sidecar spawn.

For non-interactive flows (CI provisioning, scripted setup) the
`omac register` command also accepts:

- `--fields-from <file>` — read `KEY=VALUE` lines (same wire format as
  `--secrets-from`).
- `OMAC_CONFIG_<NAME>` env vars at register time, by analogy with
  `OMAC_SECRET_<NAME>`.
- `--no-fields` — skip field prompts entirely (the inverse of
  `--no-secrets`).

`omac deregister --purge-fields` drops a skill's entries from
`skill-config.yaml` (in addition to `--purge-secrets` for the
keychain).

### 9.5 Inspecting resolved values

`omac config show <skill>` is the host-side counterpart to a sidecar's
`/whoami` endpoint: it shows what omac WOULD inject into the sidecar's
environment if `omac start` ran right now, without actually spawning
anything. Useful for "why isn't my sidecar seeing X?" debugging.

```bash
$ omac config show tng-email
skill:   tng-email
mount:   /tng-email/
workdir: /Users/you/work/tng-sandbox

config:
  NAME                    TYPE    REQ  SOURCE                     VALUE
  TNG_EMAIL_ADDRESS       string  yes  stored                     [email protected]
  TNG_EMAIL_DISPLAY_NAME  string  yes  default_from_env:USER      jane
  TNG_EMAIL_SIGNATURE     string  no   default                    Regards,
  TNG_EMAIL_IMAP_HOST     string  yes  default                    mail.tngtech.com
  TNG_EMAIL_IMAP_PORT     int     yes  default                    993
  TNG_GPG_DEFAULT_KEY     string  no   missing-optional           <missing-optional>
  TNG_GPG_REQUIRED        bool    no   stored                     false

secrets:
  NAME                REQ  FINGERPRINT
  TNG_EMAIL_PASSWORD  yes  sha256:e3b0c44298fc
  TNG_GPG_PASSPHRASE  no   <missing>
```

The `SOURCE` column tells you *which* of the resolution rungs in §9.3
produced the displayed value. `--json` emits the same data as a single
JSON object so it's pipeable into `jq`.

`omac config get <skill> <field>` prints just the resolved value,
suitable for shell-script substitution:

```bash
imap_port=$(omac config get tng-email TNG_EMAIL_IMAP_PORT)
```

This deliberately does NOT support fetching secrets — exposing a
plaintext credential to stdout (and your shell's history) defeats the
keychain. Use `omac config show --json` and inspect the fingerprint
to verify a secret is the value you expect.

The fingerprint format is `sha256(value)[:12]`, byte-for-byte
identical to the reference `echo-rest` sidecar's `/whoami` route. So a
quick visual diff between `omac config show` and `curl
$OMAC_ECHO_BASE/whoami` confirms the value the sidecar is actually
seeing matches the value omac thinks it should be injecting.

### 9.6 When to use `secrets:` vs `config:`

If the value would be embarrassing in a screenshot, use `secrets:`.
If it would be embarrassing in `git log` of a private repo, use
`secrets:`. Otherwise — base URLs, region names, retry limits, feature
flags, log verbosity — use `config:`. Anything that gets typed into a
public chat ("set my region to eu-central-1") is a config field.

### 9.7 Example: reading a config field in Python

```python
import os
GREETING = os.environ.get("ECHO_GREETING", "hello")     # string
VERBOSE  = os.environ.get("ECHO_VERBOSE", "false") == "true"
TIMEOUT  = int(os.environ.get("ECHO_MAX_TICK", "100"))  # int
MODE     = os.environ.get("ECHO_MODE", "demo")          # enum: just a string
```

The reference skill `.opencode/skills/echo-rest/` exercises every type;
its `/whoami` route echoes the resolved values back to the caller so
you can verify omac injected them correctly.

---

## 10. Distributing the skill

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

## 11. Checklist before shipping

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
