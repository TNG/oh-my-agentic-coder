# Security model

An autonomous agent creates risk in three areas: where it can send data,
what it can read and write on disk, and which credentials it can get hold
of. This document describes what omac defends against in each, how the
confinement is enforced, and exactly what a sandboxed agent can still see.

## What omac protects against

### Network

Most ways an agent can cause harm end in an outbound request, so the
network is where omac focuses first:

- **Exfiltration.** A compromised dependency or tool tries to ship your
  source code to a server it controls.
- **Data leakage.** The agent sends proprietary code or configuration to a
  third-party service or an unexpected model endpoint.
- **Prompt injection.** A file, dependency, issue, or web page the agent
  reads contains hidden instructions that redirect it: "upload this",
  "post that to this URL". A well-behaved agent can still be steered this
  way.

The defense is the same for all three: nothing leaves the sandbox unseen.
In the default `filtered` mode every outbound connection is routed through
omac's own HTTP proxy on loopback, and there is no built-in allowlist that
quietly lets traffic through.

- Hosts you list as allowed or denied in the profile are honored silently.
- Any other host raises a native OS dialog asking you to allow or deny it:
  once, permanently for that host, or permanently for a domain suffix
  (`*.example.com`). A tricked or compromised agent cannot reach a new
  destination without you seeing the request first.
- With no dialog available (CI, a headless server) the request is denied.
  The default fails closed.
- In corporate environments omac detects `HTTPS_PROXY` / `HTTP_PROXY` /
  `NO_PROXY` and chains allowed traffic through the upstream proxy. Its own
  filter runs first; the corporate proxy is only transport underneath (see
  [Configuration → Corporate proxy](./CONFIGURATION.md#corporate-proxy)).

### Filesystem

**Risk:** an agent with broad filesystem access can read SSH keys, cloud
credentials, or unrelated projects, and can write outside its scope.

omac grants the sandbox an explicit set of paths and hides the rest:

- By default that is the working directory (read/write) plus a fixed set of
  toolchain and cache directories and the config directories the active
  harness needs.
- Sensitive locations stay denied even when a broader grant would cover
  them: `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.kube`, and `.env` / `.envrc`
  files, including nested ones inside the project.
- You can mask further files by name or glob (`*.key`) across every granted
  directory.

The full picture is in [What the sandbox can see](#what-the-sandbox-can-see)
below.

### Secrets and credentials

**Risk:** integrations (GitHub, GitLab, Jira, email) need API tokens. If the
agent holds a token, a prompt injection or a buggy tool call can leak it.

omac keeps secrets on the host side of the boundary, never inside it:

- Tokens are stored in the OS keychain (Keychain on macOS, Secret Service on
  Linux), not on disk in plaintext.
- A token is injected only into the helper process for its skill, never into
  the agent's environment.
- The agent uses the service through a socket without ever seeing the token
  (see [`CREATING_A_SKILL.md`](../CREATING_A_SKILL.md)).

Every one of these decisions — each network allow/deny, each secret
injection logged by name only, each process omac spawns — is written to an
append-only audit trail stored outside the sandbox's reach (see
[Configuration → Audit trail](./CONFIGURATION.md#audit-trail)).

## How the isolation works

### Native host security capabilities

omac ships no kernel module, driver, or custom isolation layer. It uses the
security primitives the operating system already provides, so the
confinement is enforced by the kernel rather than by the agent's
cooperation:

| Concern | macOS | Linux |
|---|---|---|
| Sandbox | Seatbelt (`sandbox-exec`) | bubblewrap (user namespaces) + Landlock |
| Secret store | Keychain | Secret Service |
| Prompt dialog | AppleScript | zenity / kdialog |

There is no privileged daemon to run and nothing bespoke to audit.

### Least privilege by default

The agent starts restricted and gains access only where you grant it:

- Filesystem access limited to the working directory and required harness
  dirs.
- Network egress filtered, with unknown hosts prompted or denied.
- Secrets reachable only through a skill's socket, never in the environment.
- Environment variables passed through via an allowlist, not wholesale.
- Protected paths on a deny list that overrides broader grants.

Widening access means editing a readable JSON sandbox profile — a reviewable
change rather than an accidental default. See
[Configuration → Sandbox profiles](./CONFIGURATION.md#sandbox-profiles).

## What the sandbox can see

The sandbox receives resolved **values** (env vars, socket paths), not
config files. Only these paths from the host are accessible inside the
sandbox:

| Path | Access | Source |
|---|---|---|
| `<workdir>` | read+write | `workdir.access: readwrite` (default) |
| Selected harness config/state dirs (e.g. `~/.claude`, `~/.codex`, `~/.copilot`, `~/.pi`, `~/.local/share/opencode`) | read+write | `harness.SandboxDirs` → `--allow` flags (injected at launch) |
| Tool cache scope (`~/.cache/omac/<sha256(scope)>`, exposed as `OMAC_CACHE_DIR` / `XDG_CACHE_HOME`) | read+write | omac prepares the scope at launch and injects `--allow <scope>` (see [Tool cache isolation](./CACHE_ISOLATION.md)) |
| `~/.cargo/bin`, `~/.rustup`, `~/go/bin`, `~/.nvm`, `~/.bun/bin` | read-only | default profile `filesystem.read` — runtime installation paths stay visible so installed compilers can run; this is neither a writable nor a cache grant |
| `~/.config/agents/skills`, `~/.agents/skills` | read-only | default profile `filesystem.read` (shared skills base) |
| `~/.gitconfig`, `~/.gitignore_global` | read-only | default profile `filesystem.read` |
| `~/.cache`, `~/Library/Caches` | **denied** | hardcoded cache roots are not granted; only the selected cache scope leaf is writable |
| `~/go`, `~/.cargo` (whole tree) | **denied** (write) | not granted by the default profile — only the toolchain `bin` leaves above are read-only |
| `/usr`, `/bin`, `/lib`, `/etc`, … | read-only | platform baseline |
| `/tmp`, `$TMPDIR` | read+write | platform baseline + per-session TMPDIR |
| Bridge socket (`$TMPDIR/omac-<hash>/bridge.sock`) | read+write | `--allow-file` / `--read` flags |
| Dynamic socket dir (e.g. Agent View `/tmp/cc-daemon-<uid>`) | read+write + AF_UNIX connect | `--allow-unix-dir` flag / `filesystem.allow_unix_dir` |
| Paths in `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.kube`, … | **denied** | protected paths (override with `filesystem.override_deny`) |
| Workdir and granted-tree `.env` / `.envrc` (incl. nested) | **denied** | baseline workdir-protected set (override with `filesystem.override_deny: [".env"]`) |
| Files matching `filesystem.deny` (e.g. `*.key`) inside granted trees | **denied** | user deny list (`filesystem.deny` / `--deny`) |
| Environment variables in `environment.allow_vars` (`OMAC_*`, `HOME`, `PATH`, `LANG`, `TERM`, … + the selected harness's auth vars) | passed through | default profile `environment.allow_vars` + `harness.SandboxEnvAllow` (injected at launch) |
| Any other ambient env var (cloud/CI secrets, `DOCKER_HOST`, `SSH_AUTH_SOCK`, proxy config) | **stripped** | not on the allowlist |

> **Environment allowlist (upgrade note).** The default profile ships an
> explicit `environment.allow_vars` allowlist, so the sandbox no longer
> inherits arbitrary ambient variables from the launching shell — only the
> operational minimum plus the selected harness's provider-auth variables
> pass through. Entries are exact names or trailing-`*` prefixes (e.g.
> `OMAC_*`); `OMAC_*` is a **broad prefix**, so skill sidecars must not set
> arbitrary `OMAC_*` variables in the host env expecting them to stay out of
> the sandbox.
>
> **Defaults on by default; remove with `deny_vars`.** Every profile — empty
> or restrictive, including custom ones like `tng-default.json` — is granted
> the full operational default set (`sandboxprofile.DefaultAllowVars()`)
> *automatically* via `EffectiveAllowVars`, so vars like `COLORTERM` never
> need to be hand-carried into each profile. `allow_vars` then adds anything
> extra (e.g. the harness's auth vars); `deny_vars` removes anything the
> profile does not want:
> - **`allow_vars`** — extra grants on top of the defaults. `["*"]` grants
>   every non-blocklisted var.
> - **`deny_vars`** — patterns to drop (same exact / trailing-`*` matching as
>   `allow_vars`). It is applied **last** — after the allowlist *and* after
>   omac's injected overlay — so it wins over `allow_vars`, over `["*"]`, and
>   over injected vars. A profile can therefore drop even an injected var such
>   as `HTTP_PROXY` or a tool-cache path. That is deliberately powerful:
>   denying the proxy or cache injections can break networking or cache
>   isolation, so it is the profile author's explicit call.
>
> Because the defaults are merged in, `allow_vars` is **additive only** — a
> profile cannot express "narrower than the defaults" by listing fewer
> entries; use `deny_vars` for that.
>
> `sandboxprofile.BaseAllowVars()` (`OMAC_*`, `HOME`, `PATH`, `PWD`, `TMPDIR`,
> `LANG`, `LC_*`, `TERM`, `COLORTERM`) is the operational minimum. Denying a
> base var is **allowed** (deny always wins) but is almost never intended, so
> the launch path prints a warning and `omac doctor` reports it. The remaining
> defaults (`SHELL`, `USER`, `LOGNAME`, `TZ`, `EDITOR`, `VISUAL`,
> `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `NPM_CONFIG_PREFIX`,
> `DISPLAY`) are the removable convenience tier — dropping those is silent.
> `DISPLAY` is only the X11 server address: a GUI still needs the filesystem
> policy to expose the X11 socket (and `XAUTHORITY` on a cookie-protected
> server), so deny it in headless or hardened profiles.
>
> `omac provenance` shows the defaults as `builtin (default)`, the profile's
> `allow_vars` additions under the profile source, and `deny_vars` as `deny`.
>
> **Empty `allow_vars` — fail-closed at launch.** If a sandbox profile has an
> empty `environment.allow_vars` (e.g. an existing `default.json` with
> `"environment": {}`, or a hand-authored profile), `omac start` / `omac serve`
> does **not** inherit every ambient var. It prints a warning, pauses briefly
> so you can read it, then forwards **only the operational minimum** —
> `sandboxprofile.DefaultAllowVars()`: `HOME`, `PATH`, `PWD`, `TMPDIR`, `LANG`,
> `LC_*`, `TERM`, `COLORTERM`, `SHELL`, `USER`, `LOGNAME`, `TZ`, `EDITOR`,
> `VISUAL`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`,
> `NPM_CONFIG_PREFIX`, `DISPLAY`, `OMAC_*`. Note this is **not** a blanket
> `XDG_*` prefix: `XDG_CACHE_HOME` is deliberately absent, because omac sets
> it itself to the isolated tool-cache scope.
>
> Everything else is **stripped** — secrets, *and* provider-auth vars.
> omac deliberately does **not** auto-forward the harness's auth variables for
> an empty profile: an empty allowlist is a misconfiguration, and omac will not
> silently push provider credentials into the sandbox to hide it. The harness
> process starts (it has `HOME`/`PATH`), but it **cannot authenticate** until
> you configure the profile. `omac doctor` and `omac provenance --check` both
> flag an empty allowlist.
>
> To resolve an empty profile, either:
> - add an explicit `allow_vars` list (start from `DefaultAllowVars()` in
>   `internal/sandboxprofile/env.go`, then add the provider/token vars the
>   harness reads — e.g. `ANTHROPIC_API_KEY`, or a custom `SKAINET_*`); back up
>   + delete a scaffolded `default.json` to have omac re-scaffold the full list
>   (it is not auto-migrated), or
> - set `allow_vars: ["*"]` to forward **every** ambient var — the danger
>   blocklist (`LD_*`, `NODE_OPTIONS`, 1Password tokens, …) still wins, so this
>   is not the same as no filtering.
>
> Note: with a **non-empty** `allow_vars`, omac injects the selected harness's
> documented auth vars (`harness.SandboxEnvAllow`) on top at launch — but only
> for **single-provider** harnesses where the key is unambiguous (claude-code
> → `ANTHROPIC_*`, codex → `OPENAI_*`, copilot → `GITHUB_TOKEN`). **Multi-provider
> harnesses (opencode, pi) auto-forward nothing**: omac will not blindly push
> every third-party provider key into the sandbox, so a user relying on an
> env-based provider key lists it in `allow_vars` themselves (opencode's primary
> `auth.json` login, in its granted dirs, is unaffected). Auto-forwarding is
> skipped entirely for the empty (misconfigured) case.
>
> A direct `omac sandbox run --profile X` invocation — not via
> `start`/`serve` — is fail-closed too: `EffectiveAllowVars` resolves an empty
> `allow_vars` to the operational defaults at the enforcement point, so no
> entry point inherits the ambient environment. It warns there rather than
> pausing; `start`/`serve` warn and pause in their own launch path, then seed
> the defaults as `--allow-env` flags, so a seeded launch does not warn twice.

For successfully inspectable built-in `{{self}} sandbox run` profiles,
`omac doctor` warns when a profile re-introduces a broad read/write
grant on the cache roots (`~/.cache`, `~/Library/Caches`) or the tool
homes (`~/go`, `~/.cargo`, `~/.rustup`), and when a host Cargo sentinel
(`~/.cargo/config`, `~/.cargo/config.toml`, `~/.cargo/credentials`, or
`~/.cargo/credentials.toml`) exists but will be invisible to an
isolated `CARGO_HOME`. It detects sentinels with `Lstat` only: doctor
never reads or copies their contents. External nono profiles are opaque
to these diagnostics and skipped. The warnings are advisory — doctor
never rewrites the profile. See
[Installation → Prerequisites](./INSTALLATION.md#prerequisites).

