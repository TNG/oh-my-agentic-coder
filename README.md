# oh-my-agentic-coder (omac)

Reference Go implementation of the design described in
[`oh-my-agentic-coder.md`](./oh-my-agentic-coder.md).

`omac` bridges out-of-sandbox REST/HTTP services into a sandboxed agent-coding
environment through a single Unix-domain-socket facade. Per-skill secrets are
stored in the OS keychain and injected into sidecar processes at start time —
they never reach the sandbox.

## Layout

```
cmd/omac/                  Entrypoint.
internal/cli/              Subcommand dispatch (register/deregister/list/
                           secrets/start/doctor/version).
internal/config/           meta.yaml + oh-my-agentic-coder.json types.
internal/registry/         .opencode/sidecar.json (atomic writes, flock).
internal/keychain/         Thin wrapper over github.com/zalando/go-keyring.
internal/secrets/          Secret type (redacted Stringer, zeroize) + masked prompt.
internal/osinfo/           macos / linux / wsl detection.
internal/facade/           Unix-socket HTTP reverse proxy (SSE + upgrades).
internal/supervisor/       Sidecar lifecycle (spawn, health, shutdown).
internal/sandbox/          Templated sandbox-runtime launcher.
```

## Build

```bash
go build -o omac ./cmd/omac
```

## Test

```bash
go test ./...
```

The facade test skips automatically in environments where Unix-socket
`connect(2)` is disallowed.

## Typical workflow

```bash
# 1. Install a skill with the existing marketplace installer.
#    (Skill must declare a `sidecar:` block in its meta.yaml — see the design doc §7.)
scripts/install.sh slack

# 2. Register its sidecar in this workdir. Prompts for every declared secret
#    (masked input, stored in the OS keychain; nothing touches disk under .opencode/).
omac register slack

# 3. Inspect the install script (omac never runs it for you).
bash .opencode/skills/slack/install/install.macos.sh

# 4. (Optional) status.
omac doctor
omac list
omac secrets list slack

# 5. Launch the full stack: sidecars → facade (Unix socket) → sandbox → agent.
omac start

# Inside the sandbox the skill reaches its sidecar via the socket:
#   curl --unix-socket "$OMAC_SOCKET" http://x/slack/api/chat.postMessage ...

# 6. Rotate a secret without re-registering.
omac secrets set slack SLACK_BOT_TOKEN
```

## CLI summary

```
omac [--workdir <dir>] <subcommand> [flags] [args]

  register     Validate meta, prompt for secrets → keychain, print install
               script, add to sidecar.json. Flags:
                 --force                 replace existing registry entry
                 --reprompt-secrets      re-prompt even if secrets exist
                 --no-secrets            skip all secret prompts
                 --secrets-from <file>   KEY=VALUE file instead of prompting

  deregister   Remove from registry. Flags:
                 --purge-secrets         also delete from keychain

  list         Show registered skills with mount, secret count, binary status.

  secrets <sub> <skill> [name]
    list, set, unset, import --from <file>

  start        Spawn sidecars → bind socket → exec sandbox runtime. Flags:
                 --sandbox <profile>     pick a sandbox profile
                 --inner <cmd>           override inner_cmd
                 --no-sandbox            debug: run inner cmd directly
                 --keep-running          don't stop sidecars on exit
                 --accept-meta-changes   tolerate meta_hash drift
                 --verbose               lifecycle logging

  doctor       Sanity checks: config, registry, binaries, secrets, sandbox.
  version
```

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | success |
| `1` | generic failure |
| `2` | misuse / invalid arguments |
| `3` | configuration or metadata invalid |
| `4` | prerequisite missing (skill not installed) |
| `5` | I/O error |
| `6` | sidecar failed health check |
| `7` | sandbox exited abnormally |
| `8` | keychain access failed |
| `9` | required secret refused by user |

## Dependencies

Minimal by design:

- `github.com/zalando/go-keyring` — macOS Keychain / Secret Service / Windows
  Credential Manager abstraction.
- `golang.org/x/term` — masked-input password prompt.
- `gopkg.in/yaml.v3` — `meta.yaml` parsing.

Everything else is stdlib.

## Example skill: `echo-rest`

A working example skill lives under `.opencode/skills/echo-rest/` and is
the reference for how to write a sidecar-backed skill:

```
.opencode/skills/echo-rest/
├── meta.yaml                    sidecar block + declared secrets + health
├── sidecar.py                   stdlib-only Python HTTP server
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
calls the sidecar through the Unix socket:

```bash
export ECHO_API_KEY="demo-key-42"           # only needed for env_passthrough
omac register --no-secrets echo-rest        # (or without --no-secrets to use the keychain)
omac start --no-sandbox --inner bash -- ./demo-client.sh
```

Expected output (abridged) when run in an environment that permits
loopback `connect(2)`:

```
OMAC_SOCKET    = /tmp/omac-<hash>/bridge.sock
OMAC_ECHO_BASE = http+unix://%2Ftmp%2Fomac-<hash>%2Fbridge.sock/echo/
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
  Python `sidecar.py` as a real subprocess, routes through the facade's
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

## Running under nono

[nono](https://nono.sh) is the sandbox runtime that the default omac launcher
profile targets. This section explains exactly what needs to be configured
for the omac Unix-socket facade to be reachable from inside a nono sandbox,
with references to the relevant nono documentation pages.

### TL;DR

The default `nono` profile shipped by omac (`internal/config/launcher.go`) works
out of the box on macOS and Linux with no extra flags from the user:

```
nono run \
  --allow-cwd \
  --profile tng-sandbox \
  --allow-file  <socket-path>  \
  --read        <socket-dir>   \
  --env OMAC_SOCKET=<socket-path> \
  --env OMAC_SKILLS=<csv> \
  --env OMAC_<SKILL>_BASE=http+unix://... \
  ...
  -- opencode
```

No `--open-port`, no `--allow-domain`, no network-profile changes are
required because nono allows network by default and Seatbelt/Landlock
treat Unix-socket connect paths as filesystem access in the policy you
already grant with `--allow-file`.

### Why it works

1. **Unix sockets on macOS (Seatbelt).** Per nono's
   [macOS Seatbelt internals](https://nono.sh/docs/cli/internals/seatbelt),
   `connect(2)` on a Unix socket is classified as `network-outbound`, not a
   file operation. Since the default nono network policy is *allow*
   (see [Networking](https://nono.sh/docs/cli/features/networking#network-control)),
   the sandboxed process is free to initiate outbound Unix-socket
   connections — provided it can **open the socket path**, which is a
   separate filesystem check that we grant with `--allow-file`.
2. **Unix sockets on Linux (Landlock).** Landlock ABI v4 only filters TCP
   (by port) and does not restrict AF_UNIX at all. File-path access to the
   socket is governed by `file-read*` / `file-write*` — exactly what
   `--allow-file` grants.
3. **Path resolution.** Both kernels walk the socket's parent directory
   during `connect(2)` to resolve the inode. omac's default `nono` profile
   therefore also passes `--read {{socket_dir}}` to cover the `$TMPDIR/omac-<hash>/`
   directory (which is not part of nono's `system_read_macos` group, see
   [Profiles & Groups](https://nono.sh/docs/cli/features/profiles-groups#built-in-groups)).

The socket path is `$TMPDIR/omac-<workdir-hash>/bridge.sock` with mode `0600`
and the directory is `0700` (see the design doc §8.3). On macOS this
canonicalizes to `/private/var/folders/...`; nono's path resolution handles
the firmlink transparently.

### Combining with network-restricting flags

| nono flag/config                                           | Effect on the omac Unix socket         | What you need to do                                                         |
| ---------------------------------------------------------- | -------------------------------------- | --------------------------------------------------------------------------- |
| *(default: network allowed)*                               | Reachable                              | Nothing extra.                                                              |
| `--network-profile <name>` (e.g. `opencode`, `claude-code`) | Reachable                              | Nothing extra. Network profiles filter TCP outbound only; Unix sockets are not affected. Use the bundled `nono-netprofile` omac profile. |
| `--allow-domain …`                                         | Reachable                              | Nothing extra. Same rationale as `--network-profile`.                      |
| `--block-net`                                              | **Blocked on macOS.**                  | Do not combine `--block-net` with this design on macOS. On Linux the kernel permits AF_UNIX under Landlock, but `--block-net` installs a seccomp filter that may affect syscalls beyond TCP — test before relying on it. If you need no-TCP-out-except-the-facade, prefer `--network-profile minimal` plus domain filtering. |
| `--credential …` (reverse-proxy mode)                      | Reachable                              | Nothing extra. Credential injection is an HTTP reverse proxy on a loopback TCP port and is orthogonal to the Unix-socket facade. |
| `--upstream-proxy …` / `--upstream-bypass …`               | Reachable                              | Nothing extra.                                                              |

### Built-in omac profiles

`omac start --sandbox <name>` selects from:

| Profile             | nono flags                                                                       | Use when                                                      |
| ------------------- | -------------------------------------------------------------------------------- | ------------------------------------------------------------- |
| `nono` *(default)*  | `--allow-cwd --profile tng-sandbox --allow-file <sock> --read <sockdir>`         | Default; network allowed by host.                             |
| `nono-netprofile`   | As above plus `--network-profile opencode`                                        | Restrict outbound HTTP to nono's `opencode` profile domains.  |
| `no-sandbox-debug`  | *(no nono — runs inner command directly)*                                        | Local debugging only. Not a security boundary.                |

You can add your own profiles by creating `.opencode/oh-my-agentic-coder.json`
in the workdir. See the design doc §14 for the full launcher-config schema.

### Setting it up from scratch

1. Install nono per the
   [nono installation guide](https://nono.sh/docs/cli/getting_started/installation).
2. Copy the repository's `tng-sandbox.json` nono profile into
   `~/.config/nono/profiles/` (see `install.sh` in the workspace root or
   [Profile Authoring](https://nono.sh/docs/cli/features/profile-authoring)).
   This profile grants cwd + the paths OpenCode itself needs.
3. Install omac (`go build -o omac ./cmd/omac` in this directory, then move
   to `$PATH`).
4. `omac register <skill>` once per skill (prompts for secrets → keychain;
   see the main workflow above).
5. `omac start` launches the stack: sidecars → facade → `nono run ... -- opencode`.
6. From inside the sandbox, the agent reads `$OMAC_SOCKET` (and
   per-skill `$OMAC_<SKILL>_BASE`) and issues HTTP requests:

    ```bash
    curl --unix-socket "$OMAC_SOCKET" http://x/<skill>/status
    ```

### Debugging inside the sandbox

Use nono's built-in introspection:

```bash
# From inside the sandbox, verify the socket is reachable:
nono why --self --path "$OMAC_SOCKET" --op read --json
```

See [Policy Introspection](https://nono.sh/docs/cli/features/policy-introspection)
and [Troubleshooting](https://nono.sh/docs/cli/usage/troubleshooting) for
more. If a skill's request returns HTTP 503 with `X-Omac-Reason: sidecar-down`,
check the per-skill log under `$TMPDIR/omac-<hash>/logs/<skill>.log`.

## Not yet implemented (v0)

See the design doc's "Open questions / future work" section. Notably:

- Headless-Linux file fallback for the keychain.
- WebSocket splice robustness tests (code path exists, untested here).
- `doctor --fix` auto-remediation.
- `OMAC_KEYRING_BACKEND` override.
- Signed skill metadata verification.
