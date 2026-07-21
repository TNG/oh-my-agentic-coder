# Running under nono

[nono](https://nono.sh) is a sandbox runtime available as an alternative to
the built-in sandbox. This section explains exactly what needs to be
configured so the facade is reachable from inside a nono sandbox, with
references to the relevant nono documentation pages.

## Two transports, by design

The facade binds **both** a Unix domain socket *and* a 127.0.0.1 TCP
port on every run. Inside the sandbox the agent gets four env vars per
skill plus three top-level ones:

| Env var | Value | Notes |
| --- | --- | --- |
| `OMAC_BASE` | `http://127.0.0.1:<port>/` | TCP transport (preferred). |
| `OMAC_HOST` / `OMAC_PORT` | `127.0.0.1` / `<port>` | Components of `OMAC_BASE`. |
| `OMAC_SOCKET` | `/tmp/omac-<hash>/bridge.sock` | Unix transport (fallback). |
| `OMAC_<SKILL>_BASE` | `http://127.0.0.1:<port>/<skill>` | Per-skill TCP URL, without a trailing slash. |
| `OMAC_<SKILL>_SOCKET_BASE` | `http+unix://%2F.../<skill>` | Per-skill Unix URL, without a trailing slash. |
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

## TL;DR — what omac actually runs

```
nono run \
  --allow-cwd \
  --profile tng-sandbox \
  --allow-file <socket-path>  \
  --read       <socket-dir>   \
  --open-port  <tcp-port>     \
  -- opencode
```

`OMAC_*` transport variables and supported cache redirects are set in nono's
parent process and propagate to the inner child by default. Nono no longer
accepts a literal `--env KEY=VAL` flag; the only `--env-*` flag is
`--env-credential`, which is keystore-only. If an external nono profile sets
`environment.allow_vars`, it must include the complete required set:

```yaml
environment:
  allow_vars:
    - OMAC_*
    - XDG_CACHE_HOME
    - GOCACHE
    - GOMODCACHE
    - NPM_CONFIG_CACHE
    - PIP_CACHE_DIR
    - CARGO_HOME
```

If an external nono profile sets `environment.allow_vars`, it must also
include `OMAC_XDG_CACHE_DIR` (covered by the `OMAC_*` glob above).

External nono profiles do not receive the built-in sandbox re-exec's trusted
cache reinjection. Omitting a redirect lets that tool use its default cache
location; omac does not provide an automatic substitute.

## Built-in omac profiles

`omac start --sandbox <name>` selects from:

| Profile             | nono flags                                                                                 | Use when                                                      |
| ------------------- | ------------------------------------------------------------------------------------------ | ------------------------------------------------------------- |
| `nono`              | `--allow-cwd --profile tng-sandbox --allow-file <sock> --read <sockdir> --open-port <p>`   | The nono profile. Works under host-default network policy *and* under proxy mode auto-activated by `tng-sandbox.json`'s `custom_credentials`. Note: the compiled-in **default** launch profile is `builtin` (the omac native sandbox: Seatbelt on macOS, bubblewrap+Landlock on Linux), not `nono`. Select `nono` explicitly with `--sandbox nono` (or set `default_profile: nono` in `oh-my-agentic-coder.yaml`). |
| `nono-netprofile`   | As above plus `--network-profile opencode`                                                 | Restrict outbound HTTP to nono's `opencode` profile domains.  |
| `no-sandbox-debug`  | *(no nono — runs inner command directly)*                                                  | Local debugging only. Retains cache-scope creation and cache redirects; only `--no-sandbox` omits them. Normal OMAC transport variables and the per-launch `TMPDIR` remain available. Not a security boundary. |

You can add your own profiles by creating
`.opencode/oh-my-agentic-coder.yaml` in the workdir (or the user-global
`~/.config/omac/config.yaml`). See the design doc §14 for the full
launcher-config schema. Available placeholders: `{{socket}}`,
`{{socket_dir}}`, `{{tcp_port}}`, `{{workdir}}`, `{{skills_csv}}`,
`{{inner_cmd}}`, `{{inner_args}}`, `{{per_skill_env_flags}}`.

### Tool-cache redirection and external nono-profile limitations

For sandboxed launches, omac prepares tool-cache directories under
`~/.cache/omac/<sha256(identity)>` (persistent mode) or a per-launch temp dir
(`--ephemeral-cache`) and redirects the caches supported by `XDG_CACHE_HOME`,
`GOCACHE`, `GOMODCACHE`, `NPM_CONFIG_CACHE`, `PIP_CACHE_DIR`, and `CARGO_HOME`.

Persistent mode uses **two** scopes. The build caches (`GOCACHE`, `GOMODCACHE`,
`NPM_CONFIG_CACHE`, `PIP_CACHE_DIR`, `CARGO_HOME`) live in a **per-workdir**
scope (`OMAC_CACHE_DIR`) — untrusted project code writes here, so it stays
isolated per project. `XDG_CACHE_HOME` lives in a **per-harness** scope
(`OMAC_XDG_CACHE_DIR`), keyed by harness name and shared across every workdir:
the harness's own cache holds the plugins/extensions declared in the user's
harness config, which are user-controlled and installed once rather than
re-fetched from a registry in every project directory (a per-project re-fetch
the sandbox network policy may block). In `--ephemeral-cache` mode both scopes
are the same per-launch directory. omac injects `OMAC_CACHE_DIR`,
`OMAC_XDG_CACHE_DIR`, and `OMAC_CACHE_MODE` plus the six redirects into the
sandbox runtime's process environment, then grants only those scope leaves
read+write in the sandbox argv. Unsupported third-party tools that hardcode another
cache path need explicit profile configuration; omac does not redirect
them automatically. `~/.rustup` may be read-only runtime-visible for
toolchain execution, but it is not a writable or cache grant. See the
design doc §9 for the full mode/trust-domain table.

**External nono-profile limitation.** The filesystem boundary omac
enforces (only the selected cache scope leaf is writable for caches;
the broad `~/.cache`, `~/Library/Caches`, `~/go`, and `~/.cargo` trees
are not writable, while `~/.rustup` may be runtime-visible read-only)
is a property of the compiled-in `builtin` profile's
`default.json`, which omac scaffolds and controls. The shipped `nono`
profiles delegate filesystem grants to nono's external
`tng-sandbox.json` profile, which omac does not edit. If that
external profile still broad-grants `~/.cache` or the tool homes, the
cache env redirects still point inside `OMAC_CACHE_DIR`, but the
host trees would also be reachable — weakening the isolation. `omac
doctor` warns only when a `{{self}} sandbox run` profile (the
`builtin` default, or a custom profile that re-execs omac) is in use
and its resolved grants re-introduce a broad cache-root or tool-home
grant; external nono profiles whose command does not re-exec omac
are opaque to doctor and skipped silently. For the strongest
cache-isolation guarantee, use the default `builtin` profile.

## Combining with other nono flags

| nono flag/config                                            | Effect on the facade                              | What you need to do                                                                       |
| ----------------------------------------------------------- | ------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| *(no extra flags; default-allow network)*                   | Both transports reachable.                        | Nothing extra. Use profile `nono`.                                                        |
| `--network-profile <name>` (e.g. `opencode`, `claude-code`) | TCP reachable via `--open-port`.                  | Nothing extra. Use profile `nono-netprofile` (or add `--open-port` to your own profile).  |
| `--allow-domain …`                                          | Same as above (also activates proxy mode).        | Nothing extra.                                                                            |
| `--credential …`                                            | Same as above.                                    | Nothing extra.                                                                            |
| `--upstream-proxy …` / `--upstream-bypass …`                | Same as above.                                    | Nothing extra.                                                                            |
| `--block-net`                                               | **Both transports blocked on macOS.**             | `--open-port` *should* still allow the loopback TCP port even under `--block-net` (see nono's "Localhost IPC" docs). Untested; report any failures. The Unix socket remains blocked because of `(deny network*)`. On Linux the picture is different (Landlock filters TCP only). |

## Setting it up from scratch

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
    curl -sS "${OMAC_ECHO_BASE}/whoami"         # TCP, works under proxy mode
    curl -sS --unix-socket "$OMAC_SOCKET" \     # Unix fallback
         http://x/echo/whoami
    ```

## Debugging inside the sandbox

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
