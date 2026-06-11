# Design: Native Sandbox

## Context

omac launches an agent harness (OpenCode / Claude Code) inside a sandbox so that skills, secrets, and the host system are protected from agent mistakes. Today the sandbox is the external `nono` binary, invoked via an argv template (`internal/config/launcher.go`). We use only a fraction of nono: kernel filesystem isolation, a filtering CONNECT proxy with an interactive domain prompt, env filtering, and a handful of port openings (the omac bridge TCP port, the inner harness listen port, SSH).

nono's architecture (verified against its source) that we replicate in reduced form:

- **Proxy, not MITM**: an HTTP CONNECT tunnel proxy runs *unsandboxed* in the supervisor on `127.0.0.1:<ephemeral>`; TLS is never terminated. Filtering happens on the CONNECT hostname. DNS is resolved once by the proxy and the connection is made to the resolved IPs (anti DNS-rebinding). Cloud metadata endpoints (`169.254.169.254`, `metadata.google.internal`, `metadata.azure.internal`) and link-local resolved IPs are hard-denied and never promptable.
- **Kernel enforcement, env vars as convenience**: `HTTP_PROXY`/`HTTPS_PROXY` point at the proxy with a session token, but the real guarantee is the kernel denying all other outbound: Seatbelt `(deny network*)` + allow to the proxy port on macOS; Landlock `NetPort` rules on Linux.
- **Interactive prompt**: native dialog (osascript / zenity / kdialog) with six options (allow/deny × once/permanent-host/permanent-suffix), 60 s default timeout, default-deny on timeout or missing dialog backend, permanent decisions persisted to a learned-policy JSON.

## Goals / Non-Goals

**Goals:**

- Single-binary omac: sandbox built in, no external sandbox runtime on macOS; only `bwrap` required on Linux.
- Feature parity with our actual nono usage: fs isolation per `tng-sandbox.json`-shaped profile, domain-filtered networking with the same prompt UX, env filtering, port openings.
- Additionally support pure blocklist filtering (nono is allowlist+prompt only).
- Fix the macOS Unix-socket pain point: the bridge `bridge.sock` must work even with network deny active (we control SBPL generation, so we emit an explicit unix-socket allow instead of working around nono's blanket deny).

**Non-Goals:**

- Credential injection / reverse proxy (omac sidecars already keep secrets out of the sandbox).
- Attestation, snapshots/rollback, audit logging, agent multiplexing, policy groups/packs, profile `extends`, WSL2 support, enterprise upstream-proxy chaining, seccomp-notify supervision for pre-Landlock-v4 kernels.
- Windows native support.

## Decisions

### D1: Sandbox lives in omac as `omac sandbox run`

The launcher template mechanism stays. The default launcher profile changes from the `nono run ...` template to:

```
{{self}} sandbox run --profile default --allow-file {{socket}} --read {{socket_dir}} {{tmpdir_flags}} --open-port {{tcp_port}} -- {{inner_cmd}} {{inner_args}}
```

where `{{self}}` is the running omac executable path. This keeps `internal/sandbox/launcher.go` (template expansion + exec) almost untouched and preserves user-configurable external sandboxes. `omac sandbox run` is also usable standalone for debugging.

*Alternative considered*: in-process launch (no re-exec). Rejected: the supervisor/child split needs a process boundary anyway (proxy must outlive sandbox application; Seatbelt/Landlock are irreversible per process), and the template indirection already exists.

### D2: Process model — supervisor / stage-2 / child

```
omac sandbox run (supervisor, unsandboxed)
 ├── filtering proxy goroutine on 127.0.0.1:0
 ├── prompt handling (dialogs run from the supervisor)
 └── sandboxed child:
      macOS:  /usr/bin/sandbox-exec -p <generated SBPL> -- <inner cmd>
      Linux:  bwrap <bind args> -- /proc/self/exe sandbox stage2 -- <inner cmd>
                where stage2 applies Landlock NetPort rules, then execve(inner cmd)
```

- **macOS**: use `sandbox-exec -p` with a generated SBPL profile string. It is officially deprecated but stable, present on all supported macOS versions, and used by Bazel and Codex CLI; it avoids cgo and a custom fork/exec dance around `sandbox_init`. If Apple ever removes it, swapping to a cgo `sandbox_init` stage-2 helper is a contained change.
- **Linux**: bubblewrap provides mount-namespace filesystem isolation (replacing nono's Landlock fs rules); a re-exec'd `omac sandbox stage2` inside bwrap applies **Landlock network rules only** (handled access: `bind_tcp` + `connect_tcp`, ABI ≥ 4) via raw syscalls (no cgo, no extra deps), then execs the inner command. The host network namespace is kept (no `--unshare-net`) so loopback access to the proxy and facade works exactly as on macOS; Landlock restricts TCP connect to {proxy port, `allow_tcp_connect` ports, `open_port` ports} and bind to {`listen_port`, `open_port` ports}. This is the same enforcement model nono uses on Linux.
- Kernels without Landlock ABI v4 (< 6.7): fail closed with a clear error. Profile/CLI escape hatch `network.enforcement: "env-only"` runs with proxy env vars but no kernel guarantee, printing a prominent warning. (We drop nono's seccomp-notify supervisor — large complexity, shrinking audience.)

### D3: Proxy and filtering semantics

One Go package (`internal/netproxy`) implementing:

- **CONNECT tunnel** for HTTPS and **absolute-URI forward proxying** for plain HTTP. No TLS termination.
- **Token auth**: 256-bit hex session token in the proxy URL userinfo (`http://omac:<token>@127.0.0.1:<port>`), validated constant-time, so other host processes can't ride the proxy.
- **Filter pipeline**, in order:
  1. hard deny: metadata hostnames, and any resolved IP in 169.254.0.0/16 / fe80::/10 (incl. IPv4-mapped) — never promptable;
  2. learned permanent **deny** entries (override everything below);
  3. `deny_domain` blocklist (exact or `*.suffix`);
  4. `allow_domain` allowlist / learned permanent allows (exact or `*.suffix`);
  5. default: if prompt enabled → prompt; else if allowlist non-empty → deny; else (pure-blocklist mode) → allow.
- **DNS pinning**: resolve once, connect to the resolved IPs.
- Loopback CONNECTs are refused (in-sandbox loopback talk goes direct via `open_port`, not through the proxy).

*Alternative considered*: SNI-sniffing transparent proxy. Rejected: requires packet redirection (unavailable without netns tricks on macOS) and adds no filtering power over CONNECT for proxy-aware clients, which all our tooling is.

### D4: Interactive prompt — same UX as nono

Dialog backends: `osascript` (macOS), `zenity` then `kdialog` (Linux), plus a parallel OS notification. Text and options identical to nono with the product name swapped:

> The sandboxed process is trying to reach:
>
> `    {host}:{port}`
>
> How should omac handle this destination?

Options: `Allow once`, `Allow permanently (this host)`, `Allow permanently (*.{suffix})`, `Deny once`, `Deny permanently (this host)`, `Deny permanently (*.{suffix})` — default `Deny once`. Suffix hint = host minus leftmost label when ≥ 3 labels; IP literals unchanged. Timeout (default 60 s) kills the dialog and applies `on_unavailable` (`deny` default | `allow`); same for headless systems. Concurrent prompts for the same host coalesce. Permanent decisions are written atomically to `~/.config/omac/learned/<profile>.json` (`{"schema":1,"entries":[{"host","scope":"host"|"suffix","decision":"allow"|"deny"}]}` — same shape as nono so existing learned files can be migrated by copy).

### D5: Profile format — `tng-sandbox.json` shape, trimmed

JSON profile, loaded from an explicit path or `~/.config/omac/profiles/<name>.json`; `default` is compiled in. Unknown fields are rejected. Schema:

```jsonc
{
  "meta": { "name": "tng-sandbox" },
  "workdir": { "access": "readwrite" },          // none|read|write|readwrite
  "filesystem": {
    "allow": ["...paths..."],                     // read+write (dir or file)
    "read":  ["...paths..."],                     // read-only
    "write": ["...paths..."],                     // write-only
    "override_deny": ["~/.git-credentials"]       // punch holes through the protected-path deny set
  },
  "network": {
    "mode": "filtered",                           // filtered|blocked|open
    "allow_domain": ["github.com", "*.npmjs.org"],
    "deny_domain":  ["*.facebook.com"],
    "listen_port": [4097],
    "allow_tcp_connect": [22],
    "open_port": [],                              // localhost connect+bind (bridge port added by launcher flag)
    "network_prompt": {
      "enabled": true,
      "prompt_timeout_secs": 60,
      "on_unavailable": "deny"                    // deny|allow
    },
    "enforcement": "kernel"                       // kernel|env-only (Linux fallback)
  },
  "environment": {
    "allow_vars": ["HOME", "PATH", "OMAC_*"]      // empty/absent = all (minus blocklist)
  }
}
```

Differences vs nono: no `security.groups`, `policy.*` (except the protected-path mechanism below), `credentials`, `custom_credentials`, `env_credentials`, `extends`, `hooks`, etc. `~` and `$VAR` expansion supported; nonexistent paths skipped with a notice.

**Protected-path baseline replaces nono's implicit default groups.** nono merges its built-in `default` profile groups into *every* profile (even with `security.groups: []`): deny groups for credentials (`~/.ssh`, `~/.aws`, `~/.netrc`, ...), keychains, browser data, macOS private data, shell history/configs, plus allow groups for system read paths, temp-write paths, Homebrew, and user tools. Losing these would silently weaken the sandbox (e.g. today's installed tng-sandbox profile grants `~/Files` read — only the deny groups keep `~/Files/../.ssh`-style secrets unreadable... and more importantly any secrets *under* granted trees). We therefore hard-code an equivalent baseline: a fixed protected-path deny set + system read/temp-write baseline, with `filesystem.override_deny` as the only escape hatch (mirroring `policy.override_deny`, which the real tng-sandbox profile uses for `~/.git-credentials`). nono's `dangerous_commands` groups are NOT replicated: command blocking is deprecated in nono v0.33+ (startup-only, not kernel-enforced, bypassable by child processes) and provides no real guarantee. CLI flags (`--allow`, `--read`, `--write`, `--allow-file`, `--open-port`, `--listen-port`, `--allow-tcp-connect`, `--allow-domain`, `--deny-domain`, `--block-net`, `--workdir-access`) merge additively on top of the profile, mirroring the nono flags the launcher template already passes.

### D6: Filesystem enforcement mapping

- **macOS SBPL**: `(version 1)` `(deny default)`; allow `process-exec*`/`process-fork`; `process-info*`/signals scoped to self/same-sandbox; `(allow file-read* (subpath …))` for read/allow paths, `(allow file-write* …)` for write/allow; protected-path `(deny file-read-data …)` rules emitted *between* read-allows and write-allows (nono's ordering, so granted writes win); `file-map-executable` limited to readable paths; `file-read-metadata` on ancestor dirs for path resolution; rules emitted for both literal and canonicalized paths (`/tmp` vs `/private/tmp`); deny Keychain daemons' mach services; baseline read access to system paths (dyld caches, `/System`, `/usr/*`, Homebrew, terminfo/zoneinfo) and write access to `$TMPDIR`/`/var/folders`/`/tmp` baked into the template.
- **Linux bwrap**: `--ro-bind` for read paths, `--bind` for allow/write paths, `--ro-bind /usr /usr` (+ `/bin`, `/lib*`, `/etc` essentials), `--proc /proc`, `--dev /dev`, `--tmpfs /tmp` unless granted, `--unshare-pid --unshare-uts --unshare-ipc --die-with-parent --new-session`. No `--unshare-net` (see D2). Everything not bound is absent, which is strictly stronger than nono's Landlock-fs approach. Protected paths inside granted trees are masked with `--tmpfs`/`--ro-bind /dev/null` overlays.
- Workdir access (`workdir.access`) translates to a grant on the CWD at the given level.

### D7: Environment filtering

Child env is built from scratch (`env_clear` semantics):

1. Always-on **blocklist** (drop even if allowlisted): `LD_*`, `DYLD_*`, `BASH_ENV`, `ENV`, `CDPATH`, `GLOBIGNORE`, `BASH_FUNC_*`, `PROMPT_COMMAND`, `IFS`, `PYTHONSTARTUP`, `PYTHONPATH`, `NODE_OPTIONS`, `NODE_PATH`, `PERL5OPT`, `PERL5LIB`, `RUBYOPT`, `RUBYLIB`, `GEM_PATH`, `GEM_HOME`, `JAVA_TOOL_OPTIONS`, `_JAVA_OPTIONS`, `DOTNET_STARTUP_HOOKS`, `GOFLAGS`, `OP_SERVICE_ACCOUNT_TOKEN`, `OP_CONNECT_TOKEN`, `OP_CONNECT_HOST`, `OP_SESSION_*`.
2. Optional **allowlist** `environment.allow_vars` (exact names or trailing-`*` prefix); empty/absent means everything not blocklisted passes.
3. **Injected** vars always win: `HTTP_PROXY`/`HTTPS_PROXY`/`http_proxy`/`https_proxy` (token URL), `NO_PROXY=localhost,127.0.0.1,::1`, and pass-through of `OMAC_*` (the launcher already exports these before exec).

### D8: Port semantics (matching nono's enforcement reality)

| Profile field | Meaning | macOS (Seatbelt) | Linux (Landlock) |
|---|---|---|---|
| `listen_port` | child may bind/listen | blanket `(allow network-bind)(allow network-inbound)` — Seatbelt cannot filter by port; coarse by design | `BindTcp(port)` exact |
| `allow_tcp_connect` | direct outbound to any host on this port (e.g. SSH 22) | `(allow network-outbound (remote tcp "*:port"))` | `ConnectTcp(port)` exact |
| `open_port` | localhost connect+bind (bridge TCP port) | outbound `localhost:port` + blanket bind | `ConnectTcp`+`BindTcp` exact |
| (implicit) proxy port | only outbound route for everything else | `(allow network-outbound (remote tcp "localhost:proxyport"))` | `ConnectTcp(proxyport)` |
| bridge Unix socket | AF_UNIX connect | explicit SBPL allow for the socket path (fixes nono's blanket `deny network*` issue) | plain file access via bind mount; Landlock net rules don't affect AF_UNIX |

### D8b: Profile directory, first-start scaffolding, pages file

Profiles live in `~/.config/omac/sandbox-profiles/<name>.json` (NOT `profiles/`, which is unrelated omac config). On first start, `default.json` is scaffolded from the compiled-in defaults (pretty-printed) and loaded from disk thereafter — the compiled-in copy is only a template, never a silent fallback once the file exists. Learned website decisions move from `~/.config/omac/learned/<name>.json` to a sibling `<name>.pages.json` so a profile and its page policy travel together. All JSON written by omac is pretty-printed (2-space indent, trailing newline).

### D8c: OpenCode Desktop folders and learn mode

- `omac serve --for-opencode-desktop` reads project worktrees from OpenCode's local state (storage JSON files under `~/.local/share/opencode/storage/project/`, falling back to the `opencode.db` SQLite `project` table) and passes each existing worktree as an `--allow` flag to the sandbox. The `/` global record is skipped. This grants everything the desktop currently knows about; folders opened for the first time *during* a session still need a restart (kernel sandboxes are frozen) — learn mode covers discovery.
- `--learn` (start + serve) runs the sandbox with **unrestricted filesystem** (workdir access + a `~` and `/` grant; network/env filtering stay active) while recording filesystem activity. Recording uses platform tracing of the child process tree (macOS: `fs_usage`-style via `ktrace`/`eslogger` is too privileged — instead we use a simpler approach: periodic sampling of open file descriptors via `lsof -p` on the process group, plus the proxy-style notice hook in stage2 on Linux via `/proc/<pid>/fd`). Aggregation collapses to top-level directories outside the granted/baseline/protected sets. At exit omac prints the candidate list and asks y/n per the spec; "yes" rewrites the profile with the additions.
- fd-sampling is approximate (short-lived opens can be missed) — acceptable: learn mode is a convenience for building the allowlist, not an enforcement mechanism, and missed paths simply resurface next session.

### D9: Launcher & lifecycle integration

- `internal/config/launcher.go`: built-in profiles become `builtin` (default, the `{{self}} sandbox run ...` template above), `no-sandbox-debug` (unchanged), and `nono` retained as a named non-default template for transition.
- `omac sandbox run` parses profile+flags, starts the proxy, builds child argv per platform, forwards signals, propagates the child's exit code, and shuts the proxy down on exit. Prompt dialogs run from this supervisor (outside the sandbox), exactly like nono.
- `omac doctor` checks: bwrap presence/version and Landlock ABI on Linux; `sandbox-exec` presence on macOS; dialog backend availability.

## Risks / Trade-offs

- [`sandbox-exec` is deprecated by Apple] → It remains shipped and widely relied upon (Bazel, Codex CLI). Mitigation: isolate SBPL application behind an interface; fallback plan is a cgo `sandbox_init` stage-2.
- [Seatbelt cannot restrict bind by port] → `listen_port` is coarse on macOS (any port once one is granted) — identical to nono today; documented in the spec.
- [`allow_tcp_connect` is host-unconstrained] → e.g. port 22 allows SSH to *any* host (data exfil channel). Same as nono; profiles should keep this list minimal. Documented.
- [Landlock ABI v4 floor (kernel ≥ 6.7) without nono's seccomp fallback] → Older kernels fail closed; `enforcement: "env-only"` escape hatch with loud warning. Acceptable: our Linux users run recent kernels.
- [bwrap external dependency on Linux] → Single, ubiquitous distro package (used by Flatpak); `omac doctor` verifies it.
- [Proxy filters by CONNECT hostname, not SNI] → A malicious in-sandbox process could CONNECT to an allowed host:443 then send a different SNI. Same limitation as nono's design; mitigated by default-deny + prompt, DNS pinning, and metadata hard-denies.
- [Prompt dialogs need a GUI session] → Headless: `on_unavailable` applies (default deny). Same as nono.
- [Dropping seccomp-notify means no per-connect supervision] → Accepted scope cut; Landlock gives equivalent kernel guarantees for our port model on supported kernels.

## Behavioral differences vs nono (intentional, user-visible)

Drop-in parity is the goal for everything omac exercises; the known deviations are:

1. **No credential injection**: nono's tng-sandbox profile injects `TNG_SKILLS_BASE_URL` + a phantom `TNG_SKILL_KEY` into the sandbox via its reverse proxy. The built-in sandbox does not. Any *in-sandbox* consumer of those variables must instead go through the omac skill sidecar (which holds the real key outside the sandbox). Must be called out in migration docs.
2. **No command blocking**: nono's default groups deny `rm`, `sudo`, `npm`, ... at startup. Deprecated upstream (v0.33+, trivially bypassable, not kernel-enforced) — not replicated.
3. **Linux fs isolation is mount-namespace based**: ungranted paths are *absent* (ENOENT) rather than Landlock-denied (EACCES). Strictly stronger; error text differs.
4. **Pre-6.7 Linux kernels**: nono falls back to seccomp-notify supervision; we fail closed (or `env-only` with warning).
5. **Blocklist mode added**: pure `deny_domain` filtering without allowlist/prompt — nono cannot do this.

Everything else — fs grant semantics, protected-path denials with `override_deny`, env blocklist/allowlist, proxy + kernel network enforcement, prompt UX/options/timeout/learned policy, port semantics including macOS coarseness — is specified to match nono's observable behavior.

## Migration Plan

1. Ship `omac sandbox run` + `builtin` launcher profile behind explicit opt-in (`sandbox: builtin` in omac config) for one release; `nono` template remains default.
2. Flip the default to `builtin`; keep `nono` as a selectable template.
3. Update `opencode-nono/install.sh` (separate repo) to stop installing nono. Migrate users automatically where possible:
   - Translate `~/.config/nono/profiles/tng-sandbox.json` (the *installed* file — users accumulate extra `filesystem.allow` entries via nono's permission flow, so translate the user's copy, not the repo template) into `~/.config/omac/profiles/default.json`: copy `meta`, `workdir`, `filesystem.allow/read/write`, `policy.override_deny` → `filesystem.override_deny`, `network.listen_port/allow_tcp_connect/open_port/network_prompt`; drop `credentials`/`custom_credentials` with a notice.
   - Copy learned network decisions from nono's locations (`<profile>.learned.json` next to the profile, or `~/.config/nono/learned/<profile>.json`) to `~/.config/omac/learned/default.json` — the file format is identical.

Rollback: set launcher profile back to `nono` in omac config; no data migration to undo (learned-policy file is copied, not moved).

## Open Questions

- Should `omac sandbox run` support a `--no-proxy` mode where `network.mode: "open"` skips the proxy entirely (pure fs/env sandbox)? Leaning yes — trivial and useful for trusted networks.
- Exact macOS baseline SBPL read-allow set (dyld caches, `/Library/Preferences/.GlobalPreferences.plist`, mDNSResponder socket for DNS, etc.) — to be derived empirically during implementation; start from nono's template.
