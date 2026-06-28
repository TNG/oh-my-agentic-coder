# Native Sandbox (replace nono)

## Why

omac currently depends on the external `nono` binary for sandboxing, but we only use a small slice of its feature set (kernel filesystem isolation, filtered networking with an interactive domain prompt, env filtering, port openings). Carrying a full third-party Rust toolchain dependency for that slice costs us install complexity (brew tap, compat symlinks), upgrade coupling, and features we must work around (credential proxy auto-activation breaking Unix sockets on macOS). A native, built-in sandbox lets omac ship as a single binary with exactly the isolation we need.

## What Changes

- Add a built-in sandbox to omac, exposed as `omac sandbox run` (re-exec'd internally by the launcher), backed by **Seatbelt on macOS** and **bubblewrap (+ Landlock net rules) on Linux**.
- Add a **filtering HTTPS/HTTP CONNECT proxy** that runs unsandboxed in the supervisor process: domain-based allowlist or blocklist filtering, hard-denied cloud-metadata endpoints, DNS pinning, and token-authenticated access.
- Add an **interactive network prompt** with the same UX as nono (same dialog text, same six allow/deny options, timeout with `on_unavailable` fallback, learned-policy persistence).
- Add **filesystem isolation** configured via a profile file with the same shape as nono's `tng-sandbox.json` (`workdir.access`, `filesystem.allow/read/write`, plus `filesystem.deny` to mask files such as `.env` inside granted trees).
- Add **environment variable filtering**: always-on dangerous-variable blocklist plus optional allowlist with prefix wildcards.
- Add **port openings**: `listen_port` (bind), `allow_tcp_connect` (outbound direct, e.g. SSH on 22), `open_port` (localhost both ways — used for the omac bridge TCP port).
- Change the default launcher profile from the external `nono run ...` template to the built-in sandbox; keep the template mechanism so users can still configure external sandboxes. **BREAKING** for users relying on the implicit `nono` default.
- Explicitly out of scope (dropped nono features): credential injection / reverse proxy, attestation, snapshots/rollback, audit logs, multiplexing, policy groups/packs, WSL2-specific handling, hooks.

## Capabilities

### New Capabilities

- `sandbox-profile`: Sandbox configuration file format (JSON profile: meta, workdir, filesystem, network, environment) and the `omac sandbox run` CLI surface.
- `sandbox-process-isolation`: Kernel-enforced filesystem isolation and environment variable filtering via Seatbelt (macOS) and bubblewrap (Linux).
- `sandbox-network`: Network isolation and domain filtering — the supervisor-side filtering proxy, allowlist/blocklist modes, interactive prompt with learned policy, and port-opening rules.
- `sandbox-launch`: Launcher integration — how `omac start`/`omac serve` compose the built-in sandbox (bridge socket/TCP port grants, env propagation, lifecycle/teardown).

### Modified Capabilities

<!-- none: openspec/specs/ has no merged baseline specs; launcher behavior changes are captured in sandbox-launch -->

## Impact

- **New code**: `internal/sandbox/` grows backend packages (seatbelt profile generation, bwrap argv construction, Landlock net shim), `internal/netproxy/` (filtering CONNECT proxy + prompt), profile parsing in `internal/config/`.
- **Changed code**: `internal/config/launcher.go` (default profile becomes built-in sandbox), `internal/cli/` (new `sandbox` subcommand), `internal/cli/start.go`/`serve.go` (proxy lifecycle).
- **Dependencies**: removes runtime dependency on `nono`; adds runtime dependency on `bwrap` (Linux only); Landlock via direct syscalls (no cgo). macOS uses `sandbox-exec`/`sandbox_init` available on all supported macOS versions.
- **Install**: `opencode-nono/install.sh` no longer needs to install nono or its profile (follow-up outside this repo).
- **Platform floor**: Linux kernel-enforced network filtering requires Landlock ABI v4 (kernel ≥ 6.7); older kernels fail closed unless the profile opts into env-var-only proxy enforcement.
