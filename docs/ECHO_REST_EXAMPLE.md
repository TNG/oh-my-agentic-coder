# Example skill: `echo-rest`

This document walks through the reference skill that ships with the repo,
the integration tests that exercise the same wiring, and why streaming
(Server-Sent Events) works through the facade unchanged.

## Facade transport

`omac` bridges out-of-sandbox REST/HTTP services into the sandboxed agent
environment through facade listeners that always start on a Unix-domain
socket **and** loopback TCP. Use the TCP `OMAC_<SKILL>_BASE` URL by default;
it is required under macOS nono proxy mode, while the Unix socket remains
supported as a fallback. Sidecar ports stay private. Per-skill secrets are
stored in the OS keychain and injected into sidecar processes at start time —
they never reach the sandbox.

## The skill

A working example skill lives under `.opencode/skills/echo-rest/` and is
the reference for how to write a sidecar-backed skill. omac skills are
also valid [agentskills.io](https://agentskills.io/) skills — every
skill ships a `SKILL.md` (the agentskills.io discovery file the agent
reads via progressive disclosure) **and** an `omac.yaml` (omac's
runtime contract for the sidecar process). See
[`CREATING_A_SKILL.md`](../CREATING_A_SKILL.md) §3 for the split:

```
.opencode/skills/echo-rest/
├── SKILL.md                     agentskills.io frontmatter + Markdown
│                                instructions (name, description, when
│                                to use, endpoints, env vars)
├── omac.yaml                    sidecar block + declared secrets + health
├── scripts/
│   └── sidecar.py               stdlib-only Python HTTP server (the
│                                sidecar entry-point, referenced from
│                                omac.yaml's `command:` as
│                                `["python3", "scripts/sidecar.py"]`)
└── install/
    ├── install.macos.sh
    └── install.linux.sh
```

Exposes:

- `GET  /status`                 — health probe (facade waits on this)
- `GET  /whoami`                 — returns a sha256 **fingerprint** of the
                                   injected secret (proves injection without
                                   leaking the value)
- `POST /echo`                   — echoes back the JSON body
- `GET  /tick?n=N&gap_ms=MS`     — streaming **Server-Sent Events**; proves
                                   that the facade streams frame-by-frame
                                   instead of buffering

A companion script, `demo-client.sh`, stands in for the in-sandbox agent and
uses the supported Unix-socket fallback. Normal clients should prefer the TCP
`$OMAC_ECHO_BASE` URL, which is required under macOS nono proxy mode:

```bash
export ECHO_API_KEY="demo-key-42"           # only needed for env_passthrough
omac register --no-secrets echo-rest        # (or without --no-secrets to use the keychain)
omac start --no-sandbox --inner bash -- ./demo-client.sh
```

Expected output (abridged) when run in an environment that permits
loopback `connect(2)`:

```
OMAC_SOCKET    = /tmp/omac-<hash>/bridge.sock
OMAC_ECHO_BASE = http://127.0.0.1:<port>/echo
--- GET /echo/status ---      {"ok":true,"skill":"echo-rest"}
--- GET /echo/whoami ---      {"skill":"echo-rest","secret_present":true,"secret_fingerprint":"sha256:..."}
--- POST /echo/echo ---       {"skill":"echo-rest","secret_fingerprint":"sha256:...","you_sent":{"hello":"from sandbox","n":7}}
```

### Integration tests

Three test files exercise the same wiring in Go. Each of them skips cleanly
when the environment denies a capability it needs; together they cover the
full request matrix in any environment that permits at least one of them.

- `internal/facade/facade_test.go::TestFacadeEchoLikeRest` — in-process
  upstream reached through the facade over a Unix socket. Covers path
  rewriting, `X-Forwarded-Prefix` injection, JSON round-trip, unknown-mount
  404, facade status route, **and a 5-frame SSE stream** with incremental
  delivery assertion.
- `internal/facade/integration_test.go::TestEchoRestEndToEnd` — spawns the
  Python `scripts/sidecar.py` as a real subprocess, routes through the facade's
  Unix socket, asserts the secret was injected into the sidecar's env and
  round-trips a POST body, **and consumes the `/tick` SSE stream with the
  same incremental-delivery check**.
- `internal/facade/sse_inmemory_test.go::TestFacadeSSE_InMemory` — runs the
  facade's HTTP handler over `net.Pipe()` so no Unix socket is required;
  the upstream is a loopback `httptest` server. Exists so that SSE can be
  verified in environments that permit loopback but not Unix sockets (or
  vice-versa).

### Why SSE works

SSE is plain HTTP with a long-running response body in chunked transfer
encoding. The facade supports it without any special case because:

1. The Go reverse proxy in `internal/facade/facade.go` never reads the
   response body into memory — it streams through `http.ResponseController`
   / `Flusher` calls.
2. When the upstream sets `Content-Type: text/event-stream`, the facade
   additionally sets `X-Accel-Buffering: no` on the response so any
   downstream client libraries that inspect that header also disable
   buffering.
3. No `Content-Length` is set on an SSE response, so Go encodes it as
   chunked. Each `Flush()` on the upstream causes a chunk to be sent on
   the client socket.

The 60 ms span assertion in the tests (with a 30 ms upstream gap between
frames) guards against any future regression that would collapse the
stream into a single response write.

