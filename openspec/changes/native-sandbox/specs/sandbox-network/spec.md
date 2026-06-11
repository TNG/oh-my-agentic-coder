# sandbox-network

Network isolation, domain-based filtering via a supervisor-side proxy, interactive prompt, and port openings.

## ADDED Requirements

### Requirement: Kernel-level network deny with proxy as sole route
In `filtered` mode the sandbox SHALL deny all network access from the child at the kernel level, except: outbound TCP to the supervisor proxy on loopback, the configured port openings (`listen_port`, `allow_tcp_connect`, `open_port`), granted Unix sockets, and loopback DNS resolution facilities required by the platform. On macOS this SHALL be enforced via Seatbelt network rules (`(deny network*)` plus targeted allows, including an explicit allow for granted Unix socket paths). On Linux this SHALL be enforced via Landlock TCP rules (`connect_tcp`/`bind_tcp`, ABI ≥ 4) applied by a stage-2 re-exec inside bwrap before the inner command runs.

In `blocked` mode no proxy SHALL be started and all network access except granted Unix sockets SHALL be denied. In `open` mode no network restriction and no proxy SHALL be applied.

#### Scenario: Direct outbound blocked
- **WHEN** the child attempts a direct TCP connection to `1.2.3.4:443` (bypassing the proxy) and 443 is not in `allow_tcp_connect`
- **THEN** the connection is denied by the kernel

#### Scenario: Blocked mode
- **WHEN** `network.mode` is `blocked`
- **THEN** no proxy runs, no proxy env vars are set, and all TCP connect/bind attempts from the child fail

#### Scenario: Linux kernel without Landlock v4
- **WHEN** running on a Linux kernel without Landlock ABI ≥ 4 and `network.enforcement` is `kernel`
- **THEN** launch fails with an error explaining the kernel requirement and the `env-only` escape hatch

#### Scenario: env-only enforcement
- **WHEN** `network.enforcement` is `env-only`
- **THEN** the sandbox launches with proxy env vars but no kernel network rules, and a prominent warning states that network filtering is advisory only

### Requirement: Filtering CONNECT proxy
The supervisor SHALL run an HTTP proxy on an ephemeral loopback port, outside the sandbox. It SHALL support CONNECT tunnelling (TLS is never terminated) and absolute-URI forwarding for plain HTTP. Filtering SHALL be performed on the requested hostname. The proxy SHALL resolve DNS once per request and connect to the resolved addresses (not re-resolve), to prevent DNS-rebinding. The proxy SHALL refuse CONNECT to loopback addresses. Filtered denials SHALL return `403` with a body that names the blocked host, explicitly attributes the denial to the sandbox network policy (so it cannot be mistaken for an upstream 403; the destination is never contacted), and points at the configuration that changes the policy (prompt, `network.allow_domain`, `network.deny_domain`, the pages file). Deny responses SHALL also carry the machine-readable header `X-Omac-Sandbox: denied`.

The child SHALL receive `HTTP_PROXY`, `HTTPS_PROXY` (and lowercase variants) set to `http://omac:<token>@127.0.0.1:<port>` where `<token>` is a per-session 256-bit random hex token, and `NO_PROXY=localhost,127.0.0.1,::1`. The proxy MUST reject requests lacking the correct token (constant-time comparison).

#### Scenario: Allowed HTTPS request
- **WHEN** the child issues `CONNECT github.com:443` with the session token and `github.com` is allowed
- **THEN** the proxy establishes a raw TCP tunnel to an address `github.com` resolved to, and bytes pass through unmodified

#### Scenario: Missing token
- **WHEN** another host process connects to the proxy port without the token
- **THEN** the request is rejected with `407`

#### Scenario: Denied host
- **WHEN** the child issues `CONNECT tracker.example:443` and the filter decision is deny
- **THEN** the proxy responds `403` with the `X-Omac-Sandbox: denied` header and a body naming `tracker.example`, stating the denial comes from the sandbox policy, and no upstream connection is made

### Requirement: Filter decision order with allowlist and blocklist
For each requested hostname the proxy SHALL decide in this order:
1. **Hard deny** (never promptable, overrides everything): the hostnames `169.254.169.254`, `metadata.google.internal`, `metadata.azure.internal`, and any host whose resolved addresses include link-local ranges (169.254.0.0/16, fe80::/10, including IPv4-mapped IPv6 forms).
2. **Learned permanent deny** entries.
3. **`deny_domain`** blocklist match.
4. **Allow** if matched by `allow_domain` or a learned permanent allow.
5. **Default**: if the prompt is enabled, prompt the user; otherwise deny if `allow_domain` is non-empty (allowlist mode), else allow (pure blocklist mode).

Domain matching SHALL support exact hostnames and `*.suffix` wildcards (matching the suffix itself and any subdomain). Matching SHALL be case-insensitive.

#### Scenario: Metadata endpoint always denied
- **WHEN** the child requests `CONNECT 169.254.169.254:80`, even with prompt enabled and the user willing to allow
- **THEN** the request is denied without prompting

#### Scenario: Blocklist-only filtering
- **WHEN** `allow_domain` is empty, prompt is disabled, and `deny_domain` is `["*.facebook.com"]`
- **THEN** `CONNECT api.facebook.com:443` is denied and `CONNECT github.com:443` is allowed

#### Scenario: Allowlist filtering without prompt
- **WHEN** `allow_domain` is `["github.com"]` and prompt is disabled
- **THEN** `github.com` is allowed and any other host is denied without prompting

#### Scenario: Learned deny overrides allowlist
- **WHEN** the learned policy contains a permanent deny for `evil.example` and the profile's `allow_domain` also lists `evil.example`
- **THEN** the request is denied

### Requirement: Interactive network prompt
When `network_prompt.enabled` is true and the filter reaches the default step, the supervisor SHALL show a native dialog (macOS: `osascript`; Linux: `zenity`, falling back to `kdialog`) and fire a parallel OS notification. The dialog SHALL present the text:

> The sandboxed process is trying to reach:
>
> `    {host}:{port}`
>
> How should omac handle this destination?

with exactly six choices — `Allow once`, `Allow permanently (this host)`, `Allow permanently (*.{suffix})`, `Deny once`, `Deny permanently (this host)`, `Deny permanently (*.{suffix})` — defaulting to `Deny once`. Dialog cancellation SHALL count as `Deny once`. The `{suffix}` hint SHALL be the host with its leftmost label removed when the host has at least three labels, otherwise the host itself; IP literals are never suffix-generalized.

Concurrent requests for the same host SHALL coalesce behind a single dialog. If the dialog is not answered within `prompt_timeout_secs` (default 60), or no dialog backend is available, the `on_unavailable` policy (`deny` default, or `allow`) SHALL apply; a timed-out prompt MUST never default to allow unless `on_unavailable` is explicitly `allow`.

#### Scenario: Allow once
- **WHEN** the user picks `Allow once` for `api.example.com:443`
- **THEN** that request proceeds, no policy is persisted, and a later request for the same host prompts again

#### Scenario: Prompt timeout
- **WHEN** no choice is made within `prompt_timeout_secs` and `on_unavailable` is `deny`
- **THEN** the dialog is dismissed and the request is denied

#### Scenario: Headless system
- **WHEN** no dialog backend is available and `on_unavailable` is `deny`
- **THEN** the request is denied without hanging

### Requirement: Learned policy persistence
Permanent prompt decisions SHALL be persisted immediately and atomically (write-temp-then-rename) to `~/.config/omac/learned/<profile-name>.json` in the format `{"schema":1,"entries":[{"host":"...","scope":"host"|"suffix","decision":"allow"|"deny"}]}` (compatible with nono's learned-policy files). Persisted entries SHALL be loaded into the filter on every launch using that profile.

#### Scenario: Permanent allow persists across sessions
- **WHEN** the user picks `Allow permanently (*.npmjs.org)` and the sandbox is later restarted with the same profile
- **THEN** requests to `registry.npmjs.org` are allowed without prompting

### Requirement: Port openings
The effective profile's port lists SHALL be enforced as follows:
- `listen_port`: the child may bind/listen on these TCP ports. On Linux enforcement is per-port (Landlock `bind_tcp`). On macOS, Seatbelt cannot filter bind by port, so any non-empty `listen_port` grants bind/listen generally; this platform limitation MUST be documented.
- `allow_tcp_connect`: the child may make direct outbound TCP connections on these ports to any host (kernel cannot constrain the destination host); intended for protocols that cannot use an HTTP proxy, e.g. SSH on port 22.
- `open_port`: the child may both connect to and bind these ports on localhost; used for the omac bridge TCP port.

#### Scenario: SSH via allow_tcp_connect
- **WHEN** `allow_tcp_connect` includes 22 and the child runs `ssh git@github.com`
- **THEN** the TCP connection to port 22 succeeds without involving the proxy

#### Scenario: Bridge port reachable
- **WHEN** the launcher passes `--open-port 49152` for the facade TCP port
- **THEN** the child can connect to `127.0.0.1:49152` while other direct connections remain blocked

#### Scenario: Inner harness listen port
- **WHEN** `listen_port` includes 4097 and the inner harness binds `127.0.0.1:4097`
- **THEN** the bind succeeds
