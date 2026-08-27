# oh-my-agentic-coder (omac)

[![CI](https://github.com/TNG/oh-my-agentic-coder/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/TNG/oh-my-agentic-coder/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/TNG/oh-my-agentic-coder?sort=semver&label=release)](https://github.com/TNG/oh-my-agentic-coder/releases/latest)
[![License](https://img.shields.io/github/license/TNG/oh-my-agentic-coder)](./LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/TNG/oh-my-agentic-coder)](./go.mod)

[![Platforms](https://img.shields.io/badge/platforms-Linux%20%7C%20macOS%20%7C%20WSL2-blue)](#setup)
[![Harnesses](https://img.shields.io/badge/harnesses-opencode%20%7C%20claude--code%20%7C%20codex%20%7C%20copilot%20%7C%20pi%20%7C%20codewhale-blue)](#using-omac)

## What it is

**omac** (`oh-my-agentic-coder`) is a Go CLI that launches an agentic coding
harness (OpenCode, Claude Code, Codex, Copilot, Pi, CodeWhale) inside an OS
sandbox, while skill helpers run **on the host**. Secrets stay in the OS
keychain and are injected only into those host processes. The sandboxed agent
reaches them through one HTTP facade (loopback TCP preferred, Unix socket as
fallback). Egress is filtered; decisions and launches are appended to an audit
log the sandbox cannot write.

omac is **not** an LLM, a marketplace, or an agent. It sandboxes, bridges, and
audits. Skills and org onboarding come from elsewhere; omac starts the stack
and keeps the boundary in place.

Confinement uses OS primitives (Seatbelt on macOS; bubblewrap + Landlock on
Linux). No kernel module, no privileged daemon.

## Setup

```sh
# 1. Linux deps (macOS: Seatbelt + Keychain + AppleScript — nothing extra)
sudo apt install bubblewrap zenity libnotify-bin libsecret-1-0   # Debian/Ubuntu
sudo dnf install bubblewrap zenity libnotify libsecret           # Fedora

# 2. Install omac (pick one)
brew tap TNG-release/tap && brew trust tng-release/tap && brew install oh-my-agentic-coder   # macOS
sudo dpkg -i oh-my-agentic-coder_<version>_linux_<arch>.deb    # Debian/Ubuntu
sudo pacman -U oh-my-agentic-coder_<version>_linux_<arch>.pkg.tar.zst   # Arch

# 3. Verify
omac doctor

# 4. Optional: register a skill (secrets → OS keychain)
omac register <skill>

# 5. Launch — default builtin sandbox + default harness (opencode)
omac start
```

`omac doctor` checks sandbox, keychain, dialog backend, and harness, and prints
an actionable fix for whatever it finds. Distro-specific gotchas (Ubuntu
AppArmor and bubblewrap, WSL without a Secret Service) are in
[`docs/INSTALLATION.md`](docs/INSTALLATION.md#prerequisites). `omac start`
launches sidecars → facade → sandbox → agent. Built-in skills are
auto-provisioned on launch. For the external nono sandbox instead of builtin,
see [`docs/NONO_SANDBOX.md`](docs/NONO_SANDBOX.md).

For the full command list: `omac --help` (flags: `omac <subcommand> --help`).
Details: [`docs/CLI.md`](docs/CLI.md).

Artifacts, checksums, from-source builds, `omac update`, and the full
prerequisite matrix:
[`docs/INSTALLATION.md`](docs/INSTALLATION.md).

## Using omac

omac launches the harness inside the sandbox and exposes skills through a
stable `OMAC_*` / REST contract:

| Harness | Launch | Aliases |
|---|---|---|
| OpenCode *(default)* | `omac start` | `opencode`, `oc` |
| Claude Code | `omac start claude` | `claude-code`, `cc` |
| OpenAI Codex | `omac start codex` | `cx` |
| GitHub Copilot CLI | `omac start copilot` | `co` |
| Pi | `omac start pi` | — |
| oh-my-pi | `omac start omp` | — |
| CodeWhale | `omac start codewhale` | `cw` |

Skills are harness-agnostic. `omac continue` / `omac resume` re-enter sessions
on the same sandboxed path; `omac serve` backs OpenCode Desktop across several
project directories.

Codex runs under the sandbox on **Linux only** — its HTTP client is
incompatible with macOS Seatbelt, so `omac start codex` on macOS refuses to
start rather than hang.

**Details:** [`docs/HARNESSES.md`](docs/HARNESSES.md),
[`docs/MULTI_DIR_DESKTOP.md`](docs/MULTI_DIR_DESKTOP.md).

## Skills

A skill gives the agent a controlled path to an external service without putting
tokens in the sandbox. Typical flow:

1. **Install** the skill directory under a discovery root (e.g.
   `.agents/skills/<name>/` or `.opencode/skills/<name>/`). Org installers often
   do this for you.
2. **`omac register <skill>`** prompts for declared secrets and stores them in
   the OS keychain (`service=omac/<skill>`). Nothing under `.opencode/` holds
   plaintext tokens. Install scripts are printed for you to run — omac never
   executes them.
3. **`omac start`** launches each sidecar **outside** the sandbox with secrets
   in its environment, mounts it on the facade, and injects
   `OMAC_<MOUNT>_BASE` into the sandbox. Unregistered skills or a changed
   skill bundle refuse launch (see `--auto-register-skills` in
   [`docs/CLI.md`](docs/CLI.md) for the limited opt-in).

The agent calls the skill over the facade; the sidecar attaches credentials on
the host and talks to the upstream API.

To **author** a skill (`omac.yaml` schema, all `OMAC_*` vars, checklist), see
[`CREATING_A_SKILL.md`](./CREATING_A_SKILL.md). Worked example:
[`docs/ECHO_REST_EXAMPLE.md`](docs/ECHO_REST_EXAMPLE.md).

## Configuration

None is required — compiled-in defaults work out of the box. When you do need
to change something:

| File | Purpose |
|---|---|
| `~/.config/omac/config.yaml` (or `<workdir>/.opencode/oh-my-agentic-coder.yaml`) | Launcher: which sandbox **runtime** profile (`builtin` / `nono` / …), facade, audit, cache scope |
| `~/.config/omac/sandbox-profiles/default.json` | Grant JSON: filesystem, network mode, protected paths, env allowlist (scaffolded on first builtin start) |
| `~/.config/omac/sidecar.json` (and workdir `.opencode/sidecar.json`) | Skill registry (written by `omac register`) |
| `~/.config/omac/skill-config.yaml` | Non-secret per-skill fields |

Do not confuse the two “profile” names: launcher YAML picks **how** to sandbox;
the JSON file defines **what** the sandbox may touch.

**Details:** [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) — every field, the
audit trail format and flags, sandbox profile schema, proxy injection for
JVM/Node toolchains, and corporate-proxy chaining.
[`docs/CACHE_ISOLATION.md`](docs/CACHE_ISOLATION.md) covers the per-scope tool
caches (`cache.scope`, `--ephemeral-cache`, `omac cache clear`).

## How it works

On the host, omac loads secrets from the OS keychain into skill sidecars, then
exposes those sidecars through one HTTP facade (loopback TCP and a Unix socket).
Inside the sandbox the harness reaches skills via `OMAC_<MOUNT>_BASE`; all other
egress goes through omac's filtering proxy. Network decisions and launches are
appended to an audit log outside the sandbox.

| Term | Meaning |
|---|---|
| **Harness** | The inner agent CLI that runs inside the sandbox (`opencode`, `claude`, …). |
| **Sidecar** | A host-side HTTP process declared in a skill's `omac.yaml`. Owns secrets; never runs in the sandbox. |
| **Facade** | omac's reverse proxy. Each skill is mounted under `/{mount}/…` on loopback TCP and a Unix socket. |
| **Skill** | A package with `SKILL.md` plus an optional sidecar. Must be registered (or auto-provisioned) before `omac start` will launch. |
| **Sandbox profile** | Two namespaces: which **runtime** to use (`builtin`, `nono`, … in launcher config) vs. which **grants** apply (`~/.config/omac/sandbox-profiles/default.json`). |

**`omac start` pipeline:** load config + skill registry → refuse launch on
unregistered skills or bundle drift → load secrets from the OS keychain → spawn
sidecars and wait for health → bind the facade → exec the sandbox with `OMAC_*`
env → on exit, tear down sidecars and zeroize secrets.

Inside the sandbox, prefer TCP:

```sh
curl "$OMAC_GITHUB_BASE/repos/acme/app/issues"
```

`OMAC_<MOUNT>_BASE` is the loopback HTTP URL. `OMAC_<MOUNT>_SOCKET_BASE` /
`$OMAC_SOCKET` are the Unix-socket forms (lower overhead when available; often
blocked under Seatbelt / nono proxy mode on macOS).

## What omac protects against

| Risk | omac's answer |
|---|---|
| **Exfiltration** | Default `network.mode: filtered` — every connection through omac's loopback proxy; unknown hosts prompt (deny if no dialog) |
| **Credential theft** | `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.kube`, `.env` / `.envrc` stay denied even under broader grants |
| **Token leakage** | Tokens in the OS keychain → host sidecars only, never the sandbox env |

| Concern | macOS | Linux |
|---|---|---|
| Sandbox | Seatbelt (`sandbox-exec`) | bubblewrap + Landlock |
| Secret store | Keychain | Secret Service |
| Prompt dialog | AppleScript | zenity / kdialog |

Widening access means editing the grant JSON — a reviewable change. Full threat
model and “what the sandbox can still see”:
[`docs/SECURITY_MODEL.md`](docs/SECURITY_MODEL.md).

## Documentation

| Document | Contents |
|---|---|
| [`docs/INSTALLATION.md`](docs/INSTALLATION.md) | Every install path, updating, checksum verification, full prerequisites |
| [`docs/SECURITY_MODEL.md`](docs/SECURITY_MODEL.md) | Threat model, isolation mechanics, what the sandbox can see |
| [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) | Launcher config, audit trail, sandbox profiles, corporate proxy |
| [`docs/CACHE_ISOLATION.md`](docs/CACHE_ISOLATION.md) | Tool cache scopes, modes, cleanup, private Cargo registries |
| [`docs/CLI.md`](docs/CLI.md) | Full subcommand and flag reference, typical workflow, exit codes |
| [`docs/HARNESSES.md`](docs/HARNESSES.md) | Harness selection, session resume, bridges, skill discovery |
| [`docs/HARNESS_COMPAT.md`](docs/HARNESS_COMPAT.md) | Upstream CLI drift monitoring per harness |
| [`docs/MULTI_DIR_DESKTOP.md`](docs/MULTI_DIR_DESKTOP.md) | `omac serve` and OpenCode Desktop across directories |
| [`docs/NONO_SANDBOX.md`](docs/NONO_SANDBOX.md) | Using the external nono sandbox instead of the built-in one |
| [`CREATING_A_SKILL.md`](./CREATING_A_SKILL.md) | Authoring guide: `omac.yaml` schema, env vars, secrets, checklist |
| [`docs/ECHO_REST_EXAMPLE.md`](docs/ECHO_REST_EXAMPLE.md) | The reference `echo-rest` skill, its tests, and streaming (SSE) |
| [`docs/DEVELOP.md`](docs/DEVELOP.md) | Layout, build, test, dependencies, serve-mode dev loop |
| [`oh-my-agentic-coder.md`](./oh-my-agentic-coder.md) | The design document this implementation follows |
| [`COLLABORATION.md`](./COLLABORATION.md) | How work is tracked, branched, reviewed, and merged |

## Development

CI (`.github/workflows/ci.yml`) gates on `gofmt`, `go vet`, `staticcheck`,
build, and `go test -race`. To avoid pushing gofmt drift, install the local
pre-commit hook:

```sh
scripts/install-hooks.sh
```

The hook only runs `gofmt`. Layout, builds, and the test matrix:
[`docs/DEVELOP.md`](docs/DEVELOP.md).

## Not yet implemented (v0)

See the design doc's "Open questions / future work" section. Notably:

- Headless-Linux file fallback for the keychain.
- WebSocket splice robustness tests (code path exists, untested here).
- `doctor --fix` auto-remediation.
- `OMAC_KEYRING_BACKEND` override.
- Signed skill metadata verification.

## License

Copyright 2026 TNG Technology Consulting GmbH

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) and
[NOTICE](NOTICE) for details. You may obtain a copy of the License at
<http://www.apache.org/licenses/LICENSE-2.0>.
