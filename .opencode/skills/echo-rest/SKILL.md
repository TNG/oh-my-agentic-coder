---
name: echo-rest
description: Smoke-test REST interface backed by an out-of-sandbox HTTP sidecar. Exercises every transport and feature of the omac facade — JSON round-trip on POST /echo, secret-injection proof on GET /whoami, and a Server-Sent Events stream on GET /tick. Use when you want to verify the omac plumbing end-to-end (Unix socket + loopback TCP, SSE pass-through, env-var injection, config field types) before wiring a real third-party API skill.
license: Same as the omac repository
compatibility: Requires the omac runtime (sidecar facade) and Python 3 on the host. Inside the sandbox, only HTTP client tooling (curl, httpx, requests, …) is needed.
metadata:
  author: tngtech
  version: "0.1.0"
  omac-mount: echo
  omac-sidecar: "python3 scripts/sidecar.py"
---

# echo-rest

A reference / smoke-test skill for the [omac](../../../README.md) execution
shell. It does **not** call any real upstream service; it just bounces JSON
back at you, fingerprints an injected secret, and emits a five-frame SSE
stream. Use it to confirm that the facade, the sandbox, and your `OMAC_*`
env wiring are all healthy before you trust them with a skill that talks to
a real API.

## When to use this skill

Activate `echo-rest` when you want to:

- Verify the omac stack is running (`/status` returns `{"ok":true}`).
- Prove that omac injected the right secret without ever exposing the
  plaintext value (`/whoami` returns a `sha256(...)[:12]` fingerprint of
  `ECHO_API_KEY`).
- Round-trip a JSON body through the facade (`POST /echo`) — useful when
  diagnosing whether path rewriting or `X-Forwarded-Prefix` is broken.
- Confirm the facade streams SSE frame-by-frame instead of buffering
  (`GET /tick?n=5&gap_ms=30`).
- Inspect every `OMAC_*` env var the sandbox sees (`./demo-client.sh`
  in the repo root prints all of them).

If you're trying to actually do work against a third-party API (Slack, IMAP,
GitHub, …), you want a different skill — not this one.

## How to call it from inside the sandbox

The sidecar is reached through the omac facade. omac exports two transports
into the sandbox; **prefer the TCP loopback form** because it is the only one
that survives `nono` proxy-mode on macOS:

```bash
# TCP loopback (recommended)
curl -sS "$OMAC_ECHO_BASE/status"
curl -sS "$OMAC_ECHO_BASE/whoami"
curl -sS -X POST -H 'Content-Type: application/json' \
     -d '{"hello":"world"}' "$OMAC_ECHO_BASE/echo"
curl -sS "$OMAC_ECHO_BASE/tick?n=5&gap_ms=30"   # text/event-stream

# Unix-socket fallback (works on Linux + macOS-without-proxy-mode)
curl -sS --unix-socket "$OMAC_SOCKET" http://x/echo/status
```

`OMAC_ECHO_BASE`, `OMAC_SOCKET`, `OMAC_BASE`, etc. are populated by
`omac start`. If `OMAC_ECHO_BASE` is unset, the skill is not registered in
this workdir — run `omac register echo-rest` and re-launch.

## Endpoints

| Method | Path     | Purpose                                                                                            |
| ------ | -------- | -------------------------------------------------------------------------------------------------- |
| GET    | `/status` | Health probe. omac waits for this to return 2xx before mounting routes. Returns `{"ok":true,"skill":"echo-rest"}`. |
| GET    | `/whoami` | Returns the resolved config field values plus a `sha256(secret)[:12]` fingerprint of `ECHO_API_KEY`. The plaintext secret is never returned. |
| POST   | `/echo`   | Returns the request body wrapped under `you_sent`, plus the secret fingerprint. Useful for path-rewriting / round-trip checks. |
| GET    | `/tick`   | SSE stream of `n` events (default 3, max bounded by `ECHO_MAX_TICK`), spaced `gap_ms` milliseconds apart (default 30). Sets `Content-Type: text/event-stream` with no `Content-Length`. |

## Configuration surface

The sidecar reads its configuration from environment variables at start
time. omac populates them from `omac.yaml`'s `secrets:` and `config:`
blocks (see [`omac.yaml`](omac.yaml)). The full lifecycle and the typed
field schema are documented in [`CREATING_A_SKILL.md`](../../../CREATING_A_SKILL.md)
§9. In short:

- `ECHO_API_KEY` — declared as both a `secret` and an `env_passthrough`
  entry (the keychain wins when both are set; falling back to the
  invoking shell makes CI runs without a keychain still work).
- `ECHO_GREETING` (string), `ECHO_VERBOSE` (bool), `ECHO_MAX_TICK`
  (int), `ECHO_MODE` (enum) — exercise the four supported config-field
  types. Their resolved values are echoed back under the `config` key
  of `/whoami`, so a sandbox-side `curl` can confirm what omac
  injected.

## Verifying the wiring

The repo ships [`demo-client.sh`](../../../demo-client.sh), a shell script
that hits every route through both transports and prints every `OMAC_*` env
var it received. Run it as the inner command:

```bash
omac start --no-sandbox --inner=./demo-client.sh
```

A successful run prints (abridged):

```
OMAC_SOCKET    = /tmp/omac-<hash>/bridge.sock
OMAC_ECHO_BASE = http://127.0.0.1:<port>/echo
--- GET /echo/status ---  {"ok":true,"skill":"echo-rest"}
--- GET /echo/whoami ---  {"skill":"echo-rest","secret_present":true,"secret_fingerprint":"sha256:..."}
--- POST /echo/echo  ---  {"you_sent":{"hello":"from sandbox","n":7}, ...}
```

If any route returns `503` with header `X-Omac-Reason: sidecar-down`, the
sidecar process didn't come up — check `$TMPDIR/omac-<hash>/logs/echo-rest.log`
for its stderr.

## Notes for skill authors copying this template

`echo-rest` is also the canonical *starting point* for a new sidecar-backed
skill. Copy the directory, rename it, then in order:

1. Rewrite the frontmatter `name` and `description` to describe **your**
   skill — the `description` is what the agent uses for skill activation
   (progressive disclosure stage 1).
2. Update [`omac.yaml`](omac.yaml): change `name`, `mount`, the `command`,
   the `secrets:` and `config:` blocks. Drop `env_passthrough` for any
   real credential — declare it under `secrets:` instead.
3. Replace [`scripts/sidecar.py`](scripts/sidecar.py) with whatever HTTP
   server you want (any language, as long as it binds
   127.0.0.1:`$SIDECAR_PORT` and implements the `health.path` route).
   By convention the sidecar entry-point lives under `scripts/`, which
   is the agentskills.io directory for bundled executable code; the
   omac.yaml `command:` is resolved relative to the skill root, so
   `scripts/<your-binary>` is the typical value.
4. Edit [`install/install.macos.sh`](install/install.macos.sh) and
   [`install/install.linux.sh`](install/install.linux.sh) so they leave
   behind a working interpreter / binary. omac surfaces the path at
   register time but never executes them.
5. Read [`CREATING_A_SKILL.md`](../../../CREATING_A_SKILL.md) for the full
   schema, secrets best practices, and the pre-shipping checklist.
