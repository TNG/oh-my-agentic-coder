# oh-my-agentic-coder (omac)

Reference Go implementation of the design described in
[`oh-my-agentic-coder.md`](./oh-my-agentic-coder.md).

`omac` bridges out-of-sandbox REST/HTTP services into a sandboxed agent-coding
environment through a single Unix-domain-socket facade. Per-skill secrets are
stored in the OS keychain and injected into sidecar processes at start time —
they never reach the sandbox.

## Installation

Pre-built binaries and packages are published to
[GitHub Releases](https://github.com/TNG/oh-my-agentic-coder/releases) on every
tagged version. The release pipeline produces:

- `oh-my-agentic-coder_<version>_macOS_{x86_64,arm64}.tar.gz` — macOS binaries
- `oh-my-agentic-coder_<version>_linux_{x86_64,arm64}.tar.gz` — Linux binaries
- `oh-my-agentic-coder_<version>_linux_{x86_64,arm64}.deb` — Debian/Ubuntu (apt)
- `oh-my-agentic-coder_<version>_linux_{x86_64,arm64}.pkg.tar.zst` — Arch (pacman)
- `oh-my-agentic-coder.rb` — Homebrew formula (also bundled in the archive)
- `checksums.txt` — SHA-256 sums of every artifact

### macOS (Homebrew)

The Homebrew formula is attached to each release as a standalone artifact
(no tap is published). Install it directly from the release URL:

```sh
brew install --formula \
  https://github.com/TNG/oh-my-agentic-coder/releases/latest/download/oh-my-agentic-coder.rb
```

To pin to a specific version, replace `latest/download` with `download/v<X.Y.Z>`.

### Debian / Ubuntu (apt)

```sh
ARCH=$(dpkg --print-architecture)   # amd64 or arm64
curl -L -o omac.deb \
  "https://github.com/TNG/oh-my-agentic-coder/releases/latest/download/oh-my-agentic-coder_$(curl -s https://api.github.com/repos/TNG/oh-my-agentic-coder/releases/latest | grep tag_name | cut -d '"' -f4 | sed 's/^v//')_linux_${ARCH/amd64/x86_64}.deb"
sudo dpkg -i omac.deb
```

Or, more simply, download the `.deb` matching your architecture from the
[releases page](https://github.com/TNG/oh-my-agentic-coder/releases) and run
`sudo dpkg -i <file>.deb`.

### Arch Linux (pacman)

```sh
ARCH=$(uname -m)   # x86_64 or aarch64; map aarch64 -> arm64 in URL
curl -L -O \
  "https://github.com/TNG/oh-my-agentic-coder/releases/latest/download/oh-my-agentic-coder_<version>_linux_${ARCH}.pkg.tar.zst"
sudo pacman -U oh-my-agentic-coder_*.pkg.tar.zst
```

### Verifying downloads

Every release includes `checksums.txt`:

```sh
curl -L -O https://github.com/TNG/oh-my-agentic-coder/releases/latest/download/checksums.txt
sha256sum -c checksums.txt --ignore-missing
```

### From source

```sh
go install github.com/tngtech/oh-my-agentic-coder/cmd/omac@latest
```

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

[nono](https://nono.sh) is the sandbox runtime the default omac launcher
profile targets. This section explains exactly what needs to be
configured so the facade is reachable from inside a nono sandbox, with
references to the relevant nono documentation pages.

### Two transports, by design

The facade binds **both** a Unix domain socket *and* a 127.0.0.1 TCP
port on every run. Inside the sandbox the agent gets four env vars per
skill plus three top-level ones:

| Env var | Value | Notes |
| --- | --- | --- |
| `OMAC_BASE` | `http://127.0.0.1:<port>/` | TCP transport (preferred). |
| `OMAC_HOST` / `OMAC_PORT` | `127.0.0.1` / `<port>` | Components of `OMAC_BASE`. |
| `OMAC_SOCKET` | `/tmp/omac-<hash>/bridge.sock` | Unix transport (fallback). |
| `OMAC_<SKILL>_BASE` | `http://127.0.0.1:<port>/<skill>/` | Per-skill TCP URL. |
| `OMAC_<SKILL>_SOCKET_BASE` | `http+unix://%2F.../<skill>/` | Per-skill Unix URL. |
| `OMAC_SKILLS` | comma-separated mounts | Introspection. |

Why both:

- **TCP loopback** is the form that works on macOS under nono's *proxy
  mode* (auto-activated whenever the active nono profile defines
  `custom_credentials` — including the shipped `tng-sandbox.json`'s
  `tng_skills` block — or you pass `--network-profile`,
  `--allow-domain`, `--credential`, or `--upstream-proxy`). Proxy
  mode installs `(deny network*)` in Seatbelt, and Seatbelt classifies
  AF_UNIX `connect(2)` as `network-outbound` — so the Unix socket
  becomes unreachable. The launcher profile uses `--open-port <tcp-port>`
  to whitelist the facade's loopback port; per nono's
  [Networking](https://nono.sh/docs/cli/features/networking#localhost-ipc)
  docs that emits a Seatbelt allow rule that takes precedence over the
  blanket deny.

- **Unix socket** is the lower-overhead form and works everywhere
  *except* macOS-under-proxy-mode: on Linux it's purely
  filesystem-governed (Landlock has no AF_UNIX filter), and on macOS
  *without* proxy mode the default network policy is `allow`. We
  expose it so any agent that prefers it can still use it.

Inside the sandbox a client should prefer `OMAC_<SKILL>_BASE` (TCP)
and treat `OMAC_<SKILL>_SOCKET_BASE` as an opportunistic fallback.

### TL;DR — what omac actually runs

```
nono run \
  --allow-cwd \
  --profile tng-sandbox \
  --allow-file <socket-path>  \
  --read       <socket-dir>   \
  --open-port  <tcp-port>     \
  -- opencode
```

`OMAC_*` env vars are set in nono's parent process and propagate to the
inner child by default. (Nono no longer accepts a literal `--env KEY=VAL`
flag; the only `--env-*` flag is `--env-credential`, which is keystore-
only. If you author a custom nono profile with `environment.allow_vars`
set, add `OMAC_*` to that list or the variables will be filtered.)

### Built-in omac profiles

`omac start --sandbox <name>` selects from:

| Profile             | nono flags                                                                                 | Use when                                                      |
| ------------------- | ------------------------------------------------------------------------------------------ | ------------------------------------------------------------- |
| `nono` *(default)*  | `--allow-cwd --profile tng-sandbox --allow-file <sock> --read <sockdir> --open-port <p>`   | Default. Works under host-default network policy *and* under proxy mode auto-activated by `tng-sandbox.json`'s `custom_credentials`. |
| `nono-netprofile`   | As above plus `--network-profile opencode`                                                 | Restrict outbound HTTP to nono's `opencode` profile domains.  |
| `no-sandbox-debug`  | *(no nono — runs inner command directly)*                                                  | Local debugging only. Not a security boundary.                |

You can add your own profiles by creating `.opencode/oh-my-agentic-coder.json`
in the workdir. See the design doc §14 for the full launcher-config
schema. Available placeholders: `{{socket}}`, `{{socket_dir}}`,
`{{tcp_port}}`, `{{workdir}}`, `{{skills_csv}}`, `{{inner_cmd}}`,
`{{inner_args}}`, `{{per_skill_env_flags}}`.

### Combining with other nono flags

| nono flag/config                                            | Effect on the facade                              | What you need to do                                                                       |
| ----------------------------------------------------------- | ------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| *(no extra flags; default-allow network)*                   | Both transports reachable.                        | Nothing extra. Use profile `nono`.                                                        |
| `--network-profile <name>` (e.g. `opencode`, `claude-code`) | TCP reachable via `--open-port`.                  | Nothing extra. Use profile `nono-netprofile` (or add `--open-port` to your own profile).  |
| `--allow-domain …`                                          | Same as above (also activates proxy mode).        | Nothing extra.                                                                            |
| `--credential …`                                            | Same as above.                                    | Nothing extra.                                                                            |
| `--upstream-proxy …` / `--upstream-bypass …`                | Same as above.                                    | Nothing extra.                                                                            |
| `--block-net`                                               | **Both transports blocked on macOS.**             | `--open-port` *should* still allow the loopback TCP port even under `--block-net` (see nono's "Localhost IPC" docs). Untested; report any failures. The Unix socket remains blocked because of `(deny network*)`. On Linux the picture is different (Landlock filters TCP only). |

### Setting it up from scratch

1. Install nono per the
   [nono installation guide](https://nono.sh/docs/cli/getting_started/installation).
2. Copy the repository's `tng-sandbox.json` nono profile into
   `~/.config/nono/profiles/` (see `install.sh` in the workspace root
   or [Profile Authoring](https://nono.sh/docs/cli/features/profile-authoring)).
   This profile grants cwd + the paths OpenCode itself needs.
3. Install omac (`go build -o omac ./cmd/omac` in this directory, then
   move to `$PATH`).
4. `omac register <skill>` once per skill.
5. `omac start` launches the stack: sidecars → facade → `nono run ... -- opencode`.
6. From inside the sandbox the agent uses `$OMAC_<SKILL>_BASE`:

    ```bash
    curl -sS "${OMAC_ECHO_BASE}whoami"          # TCP, works under proxy mode
    curl -sS --unix-socket "$OMAC_SOCKET" \     # Unix fallback
         http://x/echo/whoami
    ```

### Debugging inside the sandbox

```bash
# Verify the loopback port is open:
nono why --self --host 127.0.0.1 --port "$OMAC_PORT" --json
# Verify the Unix socket is reachable (filesystem layer):
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
