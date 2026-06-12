# oh-my-agentic-coder (omac)

Reference Go implementation of the design described in
[`oh-my-agentic-coder.md`](./oh-my-agentic-coder.md).

`omac` bridges out-of-sandbox REST/HTTP services into a sandboxed agent-coding
environment through a single Unix-domain-socket facade. Per-skill secrets are
stored in the OS keychain and injected into sidecar processes at start time —
they never reach the sandbox.

## Quickstart

```sh
# 1. Install omac (pick one)
brew tap TNG-release/tap && brew install oh-my-agentic-coder   # macOS
sudo dpkg -i oh-my-agentic-coder_<version>_linux_<arch>.deb    # Debian/Ubuntu
go install github.com/tngtech/oh-my-agentic-coder/cmd/omac@latest  # from source

# 2. (Linux only) Install bubblewrap — the built-in sandbox needs it
sudo apt install bubblewrap    # Debian/Ubuntu
sudo dnf install bubblewrap    # Fedora
# macOS uses the built-in Seatbelt framework; no extra install needed.

# 3. Verify the setup
omac doctor

# 4. Register a skill (prompts for secrets → OS keychain)
omac register <skill>

# 5. Launch — default sandbox (Seatbelt/bwrap) + default harness (opencode)
omac start
```

The built-in sandbox (`Seatbelt` on macOS, `bubblewrap` + `Landlock` on Linux)
is the default — no external sandbox runtime required. To use the nono sandbox
instead, see [Running under nono](#running-under-nono).

## Choosing an inner harness

omac is harness-agnostic: it launches an inner agentic coder inside the
sandbox and exposes skills to it through a stable `OMAC_*` / REST contract. The
harness is selected by an optional **positional token** after `start` / `serve`:

```bash
omac start            # default harness (opencode) — unchanged behavior
omac start opencode   # OpenCode
omac start claude     # Claude Code
omac serve claude     # multi-directory server, Claude Code harness
```

Supported harnesses (and aliases): `opencode` (`oc`), `claude-code`
(`claude`, `cc`). Omitting the token defaults to `opencode`. An unknown token
is rejected with the list of supported names. Inner arguments that happen to be
barewords go after `--` (e.g. `omac start claude -- --model sonnet`).

Each harness ships a small client-side **bridge** that wires the agent to
omac's control plane (skill activation, the skills manifest, skill base URLs):

| Harness     | Bridge location              | Mechanism                         |
| ----------- | ---------------------------- | --------------------------------- |
| OpenCode    | `.opencode/plugins/`         | OpenCode plugin (`omac-multidir.ts`) |
| Claude Code | `.claude/` (settings + hook) | `SessionStart`/`SessionEnd` hooks |

Skills themselves are **harness-agnostic** — the same skill works unchanged
under any harness. Adding a new agentic harness means registering one
descriptor in `internal/config/harness.go` plus shipping its bridge; no
command-dispatch code changes. See `CREATING_A_SKILL.md` and
`docs/MULTI_DIR_DESKTOP.md`.

### Harness-scoped skill discovery

Each harness reads `SKILL.md` from its **own** skills directory, and omac
matches that: discovery is scoped to the active harness.

| Harness     | Own skills dir (workdir / global)              |
| ----------- | ---------------------------------------------- |
| OpenCode    | `.opencode/skills` / `~/.config/opencode/skills` |
| Claude Code | `.claude/skills` / `~/.claude/skills`            |
| *(shared)*  | `.agents/skills` / `~/.config/agents/skills`     |

- The active harness scans **its own dir + the shared `.agents/skills`**, and
  **never** the other harness's dir. So `omac start claude` ignores skills that
  live only under `.opencode/skills`, and vice versa. Put a skill in
  `.agents/skills` to share it across all harnesses.
- A skill name can be **registered once per harness** (each pointing at that
  harness's dir); registering for one harness does not disturb the other.
- The marketplace `/install` defaults to the **active harness's** dir (so
  installed skills land where that harness loads them); pass `target_path` to
  override (e.g. `.agents/skills` for a shared skill).

When a skill name is ambiguous at register time, omac stops and asks you to
pick:

```bash
omac register slack                      # if ambiguous, prints the candidates
omac register slack --harness claude     # pick the harness
omac register slack --global             # pick the user-global one over workdir
```

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

Releases are auto-published to the
[TNG-release/homebrew-tap](https://github.com/TNG-release/homebrew-tap) tap.

```sh
brew tap TNG-release/tap
brew install oh-my-agentic-coder
```

To upgrade later:

```sh
brew update
brew upgrade oh-my-agentic-coder
```

Pre-releases (tags like `v1.2.3-rc1`) are intentionally not pushed to the
tap; install those from the per-release tarball below.

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

For the project layout, build instructions (dev and release), and test
details, see [`docs/DEVELOP.md`](docs/DEVELOP.md).

## Typical workflow

```bash
# 1. Install a skill with the existing marketplace installer.
#    (Skill must declare a `sidecar:` block in its omac.yaml — see the design doc §7.)
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
omac start            # default harness (opencode)
# or: omac start claude   # launch Claude Code as the inner harness instead

# Inside the sandbox the skill reaches its sidecar via the socket:
#   curl --unix-socket "$OMAC_SOCKET" http://x/slack/api/chat.postMessage ...

# 6. Rotate a secret without re-registering.
omac secrets set slack SLACK_BOT_TOKEN
```

## CLI summary

```
omac [--workdir <dir>] <subcommand> [flags] [args]

  register     Locate the skill (workdir-local first, then user-global;
               within each layer, .agents/skills ranks above the legacy
               .opencode/skills — see CREATING_A_SKILL.md §2 for the
               full search order including XDG and legacy fallbacks),
               validate meta, prompt for secrets → keychain, prompt for
               config fields → skill-config.yaml, surface the install
               script path (omac never runs it), add to sidecar.json.
               Flags:
                 --force                 replace existing registry entry
                 --reprompt-secrets      re-prompt even if secrets exist
                 --no-secrets            skip all secret prompts
                 --secrets-from <file>   KEY=VALUE file instead of prompting
                 --reprompt-fields       re-prompt config fields
                 --no-fields             skip all config-field prompts
                 --fields-from <file>    KEY=VALUE file for fields

  deregister   Remove from registry. Flags:
                 --purge-secrets         also delete from keychain
                 --purge-fields          also delete from skill-config.yaml

  list         Show registered skills with mount, secret count, binary status.

  secrets <sub> <skill> [name]
    list, set, unset, import --from <file>

  config <sub> <skill> [args]
    show <skill> [--json]   resolved config + secret fingerprints
    get  <skill> <field>    one resolved value, suitable for $(...)

  start        Spawn sidecars → bind socket → exec sandbox runtime. Refuses
               to start if any skill is unregistered in any of the search
               roots (workdir-local .agents/skills + .opencode/skills,
               plus the user-global layers), or if a registered skill's
               bundle changed since register, or if a required config
               field is unresolvable. Auto-deregisters
               (silently) skills whose directory has vanished; secrets +
               config persist for safety. Flags:
                 --sandbox <profile>     pick a sandbox profile
                 --inner <cmd>           override inner_cmd
                 --no-sandbox            debug: run inner cmd directly
                 --keep-running          don't stop sidecars on exit
                 --accept-skill-changes  tolerate bundle_hash drift
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
- `gopkg.in/yaml.v3` — `omac.yaml` parsing.

Everything else is stdlib.

## Authoring a skill

If you want to build a new skill from scratch — or just get a deeper
walkthrough of the schema, the sidecar contract, and the dev loop — see
[`CREATING_A_SKILL.md`](./CREATING_A_SKILL.md). It covers the on-disk
layout, the full `omac.yaml` schema, every env var omac sets in the
sidecar and inside the sandbox, secrets best practices, and a
pre-shipping checklist.

## Example skill: `echo-rest`

A working example skill lives under `.opencode/skills/echo-rest/` and is
the reference for how to write a sidecar-backed skill. omac skills are
also valid [agentskills.io](https://agentskills.io/) skills — every
skill ships a `SKILL.md` (the agentskills.io discovery file the agent
reads via progressive disclosure) **and** an `omac.yaml` (omac's
runtime contract for the sidecar process). See
[`CREATING_A_SKILL.md`](./CREATING_A_SKILL.md) §3 for the split:

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

## Using the nono sandbox

omac uses a built-in sandbox by default (Seatbelt on macOS, bubblewrap +
Landlock on Linux). You may want the [nono](https://nono.sh) sandbox instead
if you need nono's credential injection, network profiles with interactive
domain prompts, or are migrating from an existing nono setup. Select it with
`omac start --sandbox nono` (or `--sandbox nono-netprofile` for domain-filtered
outbound HTTP).

See [`docs/NONO_SANDBOX.md`](docs/NONO_SANDBOX.md) for the full setup guide,
transport details (TCP vs Unix socket under proxy mode), flag combinations,
and debugging instructions.

## Not yet implemented (v0)

See the design doc's "Open questions / future work" section. Notably:

- Headless-Linux file fallback for the keychain.
- WebSocket splice robustness tests (code path exists, untested here).
- `doctor --fix` auto-remediation.
- `OMAC_KEYRING_BACKEND` override.
- Signed skill metadata verification.
