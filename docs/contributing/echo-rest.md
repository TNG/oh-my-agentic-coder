---
title: echo-rest skill
description: Reference smoke-test skill for verifying omac plumbing
---

The `echo-rest` skill is a smoke-test and reference implementation that ships with the repo. It does not call any real upstream service. Instead, it bounces requests back, fingerprints an injected secret without leaking it, and emits Server-Sent Events (SSE) to prove the facade streams responses frame-by-frame.

## When to use it

- Verify the omac stack starts and the facade is reachable.
- Prove a skill's secret reaches its own sidecar — and never the sandbox.
- Round-trip a JSON body end-to-end through the facade.
- Confirm SSE streaming is not buffered by the facade.
- Inspect all `OMAC_*` env vars that the sandbox receives.

## Endpoints

| Method | Path | What it does |
|--------|------|--------------|
| `GET` | `/status` | Health probe — facade waits on this before marking the skill ready |
| `GET` | `/whoami` | Returns a SHA-256 fingerprint of the injected secret |
| `POST` | `/echo` | Echoes the JSON request body back verbatim |
| `GET` | `/tick` | SSE stream; accepts `?n=N&gap_ms=MS` to control frame count and cadence |

## Calling it from inside the sandbox

Inside the sandbox, omac sets `$OMAC_ECHO_BASE` to the skill's URL — normally the agent calls it, but you can get a sandboxed shell yourself with `omac start --inner=bash`. Append an endpoint to it:

```bash
# Health check
curl "$OMAC_ECHO_BASE/status"

# Verify secret injection (fingerprint only, value never exposed)
curl "$OMAC_ECHO_BASE/whoami"

# Round-trip a JSON body
curl -X POST "$OMAC_ECHO_BASE/echo" \
  -H "Content-Type: application/json" \
  -d '{"hello":"from sandbox","n":7}'

# Consume an SSE stream (5 frames, 30 ms apart)
curl -N "$OMAC_ECHO_BASE/tick?n=5&gap_ms=30"
```

## Verifying the full stack

`demo-client.sh` (at the repo root) stands in for the agent and calls all four endpoints, so you can confirm the sidecar and facade work end-to-end:

```bash
omac register echo-rest                     # prompts for the demo secret, stored in the keychain
omac start --no-sandbox --inner=./demo-client.sh
```

`--inner` runs `demo-client.sh` in place of the agent, and `--no-sandbox` drops the sandbox layer so this stays a pure plumbing check — facade, sidecar, and secret injection — rather than a test of the sandbox boundary.

Expected output (abridged):

```
OMAC_SOCKET    = /tmp/omac-<hash>/bridge.sock
OMAC_ECHO_BASE = http://127.0.0.1:<port>/echo
--- GET /echo/status ---   {"ok":true,"skill":"echo-rest"}
--- GET /echo/whoami ---   {"skill":"echo-rest","secret_present":true,"secret_fingerprint":"sha256:..."}
--- POST /echo/echo ---    {"skill":"echo-rest","secret_fingerprint":"sha256:...","you_sent":{"hello":"from sandbox","n":7}}
```
