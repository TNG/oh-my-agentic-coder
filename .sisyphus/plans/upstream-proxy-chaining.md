# Plan: Upstream Proxy Chaining for omac's Built-in Sandbox

## Problem

In corporate setups, the LLM API is only reachable through a corporate proxy
(`HTTPS_PROXY` set in the host environment). omac's built-in sandbox runs its
own filtering HTTP CONNECT proxy (`internal/netproxy/server.go`) and
unconditionally injects `HTTP_PROXY`/`HTTPS_PROXY` → omac proxy URL into the
sandboxed child. The omac proxy then dials upstream targets directly
(`dialPinned` uses a plain `net.Dialer.DialContext`), so it can't reach the LLM
through the corporate proxy. "Enterprise upstream-proxy chaining" was listed as
a non-goal in the original native-sandbox design — this plan implements it.

## Architecture (Oracle-validated)

### Core idea: `Dialer` abstraction

Replace the single `dialPinned` function with a `Dialer` interface. Two
implementations: `directDialer` (current logic: hostname→resolve→pin IPs→dial)
and `upstreamProxyDialer` (dial upstream proxy, send CONNECT, parse 200, return
conn). The server calls `s.dialer.DialTunnel(ctx, host, port)` in both
`handleConnect` and `handleForward`.

### Config source: env auto, profile overrides

Resolution order:
1. `network.upstream_proxy` (new profile field) if set
2. Else host env `HTTPS_PROXY`/`HTTP_PROXY`
3. `network.no_proxy` (new profile field) if set, else host `NO_PROXY`

Corporate users with `HTTPS_PROXY` already set get zero-config. The profile
field exists for cases where the sandbox needs a different upstream than the
host.

### NO_PROXY bypass

The omac filter **always** runs first (hard-deny metadata, allow/deny lists,
interactive prompt). After the filter allows, `NO_PROXY` only selects between
"dial target directly" (current `dialPinned`) and "chain through upstream proxy
+ CONNECT/forward". `NO_PROXY` never bypasses the omac filter.

### DNS resolution

- **Through upstream proxy**: pass the **hostname** (not pre-resolved IPs) to
  the upstream proxy. Corporate proxies resolve internal hostnames the
  supervisor can't see. Anti-DNS-rebinding is unavoidably delegated here — the
  filter operates on the hostname the child requested, which is the security
  boundary.
- **Direct (no upstream or NO_PROXY match)**: keep the current anti-rebinding
  behavior — resolve locally, pin IPs, dial IPs.

### Upstream proxy auth

- **Basic auth from the URL** (`http://user:pass@proxy:8080`): parse userinfo,
  emit `Proxy-Authorization: Basic <base64>` on both CONNECT and forwarded
  requests.
- **NTLM/Kerberos**: out of scope. Document the workaround: run a local bridge
  (`cntlm` / `px`) that handles NTLM and exposes plain Basic → point
  `network.upstream_proxy` at it.

### Filter ordering (unchanged)

```
1. omac filter (filter.Check)     — always, unconditionally
2. NO_PROXY match?                — chain vs direct selection (transport only)
3a. direct:  dialPinned(addrs)    — current logic (resolve, pin, dial IPs)
3b. chained: dial upstream proxy  — CONNECT for HTTPS, absolute-URI for HTTP
```

### Error handling

On upstream-proxy dial failure or CONNECT rejection (non-200 from upstream):
return `502 Bad Gateway` with:
- Names the upstream proxy host (not the target) as the failing hop
- No credentials or proxy URL userinfo
- Upstream's status line / error if it sent one
- `X-Omac-Sandbox: upstream-error` header (parallel to existing `denied`)

Log the full error (credential-free URL) via `logf` for diagnostics.

## Implementation tasks

### Task 1: Profile schema — `network.upstream_proxy` + `network.no_proxy`
**File**: `internal/sandboxprofile/profile.go`

- Add `UpstreamProxy string` (`json:"upstream_proxy,omitempty"`) to `Network`
- Add `NoProxy []string` (`json:"no_proxy,omitempty"`) to `Network`
- Add validation in `Validate()`:
  - If `UpstreamProxy` set: must parse as URL with scheme `http` or `https`
  - `NoProxy` entries must be non-empty strings
- Add to the compiled-in default profile comment block in the package doc
- Update the scaffolded `default.json` defaults if applicable

### Task 2: `Dialer` interface + `directDialer`
**File**: `internal/netproxy/dialer.go` (new)

```go
type Dialer interface {
    DialTunnel(ctx context.Context, host string, port int) (net.Conn, error)
}
```

- `directDialer`: wraps the current `dialPinned` logic (resolve via filter's
  addrs, dial IPs in order). Actually `dialPinned` already takes `addrs` — the
  direct dialer will take host+port, resolve internally, and dial. Or: keep
  `dialPinned` as-is and have the direct dialer delegate to it.
- The `Dialer` needs access to the `Filter` for DNS resolution (the filter
  resolves + returns pinned addrs). Design: `directDialer` holds a reference to
  the filter, calls `filter.Check(ctx, host, port)` to get addrs, then dials.

### Task 3: `upstreamProxyDialer`
**File**: `internal/netproxy/dialer.go` (same file)

```go
type upstreamProxyDialer struct {
    proxyURL  *url.URL   // parsed upstream proxy URL (may contain userinfo)
    proxyAuth string     // "Basic <base64>" if userinfo present, else ""
    logf      func(string, ...any)
}
```

- `DialTunnel(ctx, host, port)`:
  1. Dial TCP to `proxyURL.Host`
  2. Write `CONNECT host:port HTTP/1.1\r\nHost: host:port\r\n[Proxy-Authorization]\r\n\r\n`
  3. Read response with `bufio.Reader` + `http.ReadResponse` (handle extra headers)
  4. If status != 200 → close conn, return error with upstream status line
  5. Return the established conn (raw tunnel, TLS bytes pass through)
- Does NOT resolve DNS locally — passes hostname to the upstream proxy
- For plain HTTP forward (`handleForward`): the dialer returns a conn to the
  upstream proxy; `handleForward` writes the absolute-URI request (not
  origin-form) to it, with `Proxy-Authorization` for the upstream, stripping
  the omac session token.

### Task 4: Wire `Dialer` into `Server`
**Files**: `internal/netproxy/server.go`, `internal/netproxy/dialer.go`

- Add `dialer Dialer` field to `Server`
- `NewServer` signature changes: accept a `Dialer` (or a `dialerFactory`)
- `handleConnect` (line 233): replace `dialPinned(ctx, addrs, port)` with
  `s.dialer.DialTunnel(ctx, host, port)`. The filter still runs first
  (`filter.Check`) — if denied, return 403 before dialing. If allowed, the
  dialer decides direct vs chained.
- `handleForward` (line 275): replace `dialPinned(ctx, addrs, port)` with
  `s.dialer.DialTunnel(ctx, host, port)`. Branch on dialer type:
  - `directDialer`: current origin-form rewrite (lines 282-289)
  - `upstreamProxyDialer`: send absolute-URI request with upstream
    `Proxy-Authorization`, strip omac session token
- The `Filter.Check` still runs in both handlers — the dialer is a transport
  layer underneath the filter.

**Key concern**: `Filter.Check` currently returns `addrs` (pinned IPs). With
the upstream dialer, `addrs` is unused (hostname goes to upstream). The filter
still needs to run for allow/deny decisions, but the dialer ignores `addrs`
when chaining. Design: the dialer's `DialTunnel` doesn't take `addrs` — it takes
`host, port`. The filter runs in the handler before calling the dialer. The
dialer resolves internally (direct) or delegates to upstream (chained).

### Task 5: `ProxyConfig` resolution in `buildProxy`
**File**: `internal/sandboxrun/run.go`

- In `buildProxy`, after loading the profile:
  1. Resolve upstream proxy URL: profile `network.upstream_proxy` → host env
     `HTTPS_PROXY`/`HTTP_PROXY`
  2. Resolve NO_PROXY list: profile `network.no_proxy` → host env `NO_PROXY`
  3. If upstream proxy configured:
     - Parse URL, extract Basic-auth userinfo
     - Build `upstreamProxyDialer`
  4. Else: build `directDialer`
  5. Pass the dialer to `NewServer`
- Use `golang.org/x/net/http/httpproxy` for canonical NO_PROXY matching (suffix,
  CIDR, `*` wildcard). Add this dep — it's already transitively available via
  `net/http`.

### Task 6: `Proxy-Authorization` handling
**File**: `internal/netproxy/dialer.go`, `internal/netproxy/server.go`

- `upstreamProxyDialer.DialTunnel`: emit `Proxy-Authorization: Basic <base64>`
  in the CONNECT request to the upstream proxy (if userinfo present in proxyURL)
- `handleForward` upstream branch: set `Proxy-Authorization` for the upstream on
  the forwarded absolute-URI request
- **Critical**: strip the child's `Proxy-Authorization` header (the omac session
  token) in both paths. Current code already strips it for the direct path
  (line 283). The upstream path must strip + replace, not just strip.

### Task 7: 502 error responses
**File**: `internal/netproxy/server.go`

- On `upstreamProxyDialer.DialTunnel` error: `writeRawResponse(conn, 502, "X-Omac-Sandbox: upstream-error\r\n", body)`
  where body names the upstream proxy host and includes the upstream's status
  line if available.
- Do NOT include credentials or proxy URL userinfo in the body.
- Log full error via `logf`.

### Task 8: Tests
**File**: `internal/netproxy/dialer_test.go` (new), `internal/netproxy/server_test.go`

- `directDialer` test: resolve + dial (existing behavior, regression check)
- `upstreamProxyDialer` test:
  - Spin up an `httptest.Server` that accepts CONNECT and responds 200
  - Assert the dialer dials it, sends CONNECT, parses 200, returns a working conn
  - Assert `Proxy-Authorization` is emitted when URL has userinfo
  - Assert 502 / error on non-200 from upstream (e.g. 407 Proxy Auth Required)
  - Assert hostname (not IP) is sent in CONNECT target
- Server-level test:
  - `handleConnect` through an upstream proxy → end-to-end splice
  - `handleForward` through an upstream proxy → absolute-URI forwarding
  - NO_PROXY match → direct dial (not chained)
  - NO_PROXY non-match → chained
  - Filter denies before chaining (no upstream dial on deny)
- `buildProxy` test:
  - Env `HTTPS_PROXY` set → `upstreamProxyDialer` selected
  - Profile `upstream_proxy` set → overrides env
  - Neither set → `directDialer`
  - `no_proxy` matching: suffix, CIDR, `*`

### Task 9: Profile validation tests
**File**: `internal/sandboxprofile/profile_test.go`

- `upstream_proxy` with invalid URL → validation error
- `upstream_proxy` with non-http scheme → validation error
- `no_proxy` with empty entry → validation error
- Valid `upstream_proxy` + `no_proxy` → accepted

### Task 10: Documentation
**Files**: `README.md`, `oh-my-agentic-coder.md`, `AGENTS.md`

- README: new "Corporate proxy" section under Configuration:
  - omac auto-detects `HTTPS_PROXY`/`HTTP_PROXY` from the host env
  - Override via `network.upstream_proxy` in sandbox profile
  - `NO_PROXY` support
  - NTLM/Kerberos: use a local bridge (`cntlm`/`px`), point `upstream_proxy` at it
- `oh-my-agentic-coder.md`: update the non-goals section (remove "enterprise
  upstream-proxy chaining" or mark as implemented)
- `openspec/changes/native-sandbox/design.md`: update non-goals

## Key design decisions (Oracle-validated)

1. **Single `Dialer` interface** — keeps transport decision in one place, both
   handlers call the same abstraction, no chain logic duplication.
2. **Env-first, profile-overrides** — matches Go ecosystem conventions
   (`http.ProxyFromEnvironment`), zero-config for corporate users.
3. **Hostname to upstream, pinned IPs for direct** — preserves anti-DNS-rebinding
   where enforceable, accepts unavoidable delegation where not.
4. **No NTLM/Kerberos** — keeps stdlib + `x/net/httpproxy` dep only, avoids
   connection-pooling complexity incompatible with connection-per-request model.
5. **Filter always first** — the upstream proxy is purely a transport underneath
   the filter. NO_PROXY only affects chain-vs-direct, never filter decisions.

## Risks / watch-outs

- **`Proxy-Authorization` leakage**: child sends `Proxy-Authorization: Basic
  omac:<token>` to the omac proxy. When forwarding to upstream, must strip this
  and replace with upstream's credentials. Current code already strips for direct
  path (line 283); upstream path must strip + replace.
- **CONNECT response parsing**: corporate proxies may send extra headers
  (`Proxy-Authenticate`, `Connection: keep-alive`) before/after 200. Use
  `http.ReadResponse`-like parsing, not just first-line.
- **`httpproxy` NO_PROXY semantics**: `x/net/http/httpproxy` treats entries as
  suffix-match on hostname or CIDR — stricter than `allow_domain`'s `*.suffix`
  wildcard syntax. Document the difference or align them.
- **PAC files**: out of scope for v1. Document the local-bridge workaround.

## Effort estimate

Medium (1-2 days): ~8 tasks, each 30min-2h. The `Dialer` abstraction is the
linchpin — once it's in place, the rest is wiring + tests.
