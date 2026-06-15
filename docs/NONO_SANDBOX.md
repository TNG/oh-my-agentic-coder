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

`OMAC_*` env vars are set in nono's parent process and propagate to the
inner child by default. (Nono no longer accepts a literal `--env KEY=VAL`
flag; the only `--env-*` flag is `--env-credential`, which is keystore-
only. If you author a custom nono profile with `environment.allow_vars`
set, add `OMAC_*` to that list or the variables will be filtered.)

## Built-in omac profiles

`omac start --sandbox <name>` selects from:

| Profile             | nono flags                                                                                 | Use when                                                      |
| ------------------- | ------------------------------------------------------------------------------------------ | ------------------------------------------------------------- |
| `nono` *(default)*  | `--allow-cwd --profile tng-sandbox --allow-file <sock> --read <sockdir> --open-port <p>`   | Default. Works under host-default network policy *and* under proxy mode auto-activated by `tng-sandbox.json`'s `custom_credentials`. |
| `nono-netprofile`   | As above plus `--network-profile opencode`                                                 | Restrict outbound HTTP to nono's `opencode` profile domains.  |
| `no-sandbox-debug`  | *(no nono — runs inner command directly)*                                                  | Local debugging only. Not a security boundary.                |

You can add your own profiles by creating
`.opencode/oh-my-agentic-coder.yaml` in the workdir (or the user-global
`~/.config/omac/config.yaml`). See the design doc §14 for the full
launcher-config schema. Available placeholders: `{{socket}}`,
`{{socket_dir}}`, `{{tcp_port}}`, `{{workdir}}`, `{{skills_csv}}`,
`{{inner_cmd}}`, `{{inner_args}}`, `{{per_skill_env_flags}}`.

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
