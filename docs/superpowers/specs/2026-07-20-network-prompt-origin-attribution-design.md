# Origin attribution for sandbox network prompts

**Date:** 2026-07-20
**Status:** Design. Not yet implemented.
**Companion:** [Intent declarations for sandbox prompts](2026-07-07-intent-declarations-design.md)
**Data dependency:** `internal/netprompt/hostmap/opencode-egress.json` (host→subsystem map, already landed).

## Problem

In `filtered` network mode, a host that is not allowlisted and not yet learned
produces an interactive popup (`internal/netproxy/filter.go`, step 5). For
opencode this fires constantly on **background, non-user-initiated** dials —
tree-sitter grammars, LSP-server downloads, the model catalog, themes. The user
opens a Kotlin file and gets asked to approve `raw.githubusercontent.com:443`, a
host neither they nor the agent named, with:

```
Agent intent: (not declared)
```

The intent line is empty *by construction*: the LLM never dialed this — opencode's
runtime did. So the popup reads as "something is phoning home for no reason," and
users are unsettled. Silently allowlisting the hosts (the obvious alternative) hides
the traffic and weakens the guardrail's "you approve what leaves the sandbox" story.

## Goal

Fill the same dialog with an **auto-resolved** attribution block when the agent
did not declare intent — say *who* is dialing and the *likely cause*, without
weakening the guardrail:

```
The sandboxed process is trying to reach:

    raw.githubusercontent.com:443

Origin:       opencode  (pid 13597)
Likely cause: syntax-highlighting grammar / query (tree-sitter)
Agent intent: (not declared)

How should omac handle this destination?
```

Two independent, best-effort signals, resolved **only on the prompt path**:

1. **Origin** — the process that opened the connection, from its source port.
2. **Likely cause** — the host's purpose, from the embedded host map.

Both are advisory (like intent): shown before a decision, never auto-granting.

## What this can and cannot do

Established from a runtime audit + the opencode source (see the host-map README):

- **Cause from hostname: yes, coarse.** opencode partitions egress by host, so the
  hostname resolves a reliable cause without seeing the URL.
- **Exact URL: no.** The proxy is a raw CONNECT tunnel; the request path is never
  visible. Not solvable without terminating TLS, which omac does not do.
- **opencode-core vs the agent within: not by process.** They share one runtime
  (one PID dials `api.anthropic.com`, `github.com`, and telemetry). PID *can*
  separate opencode-core from a tool subprocess it spawns (an LSP server, ripgrep).

## Non-goals

- **URL-path display.** Descoped for the same reason as the intent spec: CONNECT
  never carries the path.
- **macOS process attribution in v1.** The host-map cause line is cross-platform and
  ships everywhere; `/proc`-based origin is Linux-first. macOS (`libproc`/`lsof`) is
  a follow-up. On macOS v1 the Origin line is simply omitted, the cause line remains.
- **Correlating opencode's debug log** for exact-URL precision. Brittle coupling;
  revisit only if the coarse cause proves insufficient.
- **Forcing/altering decisions.** Attribution changes only the dialog text. Filter
  order, learned store, and default selection (`Deny once`) are untouched.
- **Non-opencode harnesses.** The map is opencode-derived. Hosts like
  `github.com`/`registry.npmjs.org` generalize, but codex/copilot maps are out of
  scope here.

## Architecture

Two resolvers live in the **sandbox child** (`omac sandbox run`), alongside the
existing prompter, and are consulted by it at prompt time. No new process, no
supervisor round-trip (unlike the intent registry, which must cross processes).

```
   handleConnect (has conn.RemoteAddr = child src addr)
        │  src netip.AddrPort threaded down
        ▼
   Filter.Check ──► defaultDecision ──► promptCoalesced ──► Prompter.Prompt(PromptRequest)
                                                                 │
                                                 ┌───────────────┴───────────────┐
                                                 ▼                               ▼
                                        origin.Resolver                    hostmap.Map
                                     (src port → pid → comm)          (host → cause string)
                                        Linux /proc; else ∅            embedded JSON
```

Resolution runs **only when a prompt is actually shown** — never on the allow/deny
hot path — so there is zero per-request cost for the common allowed/denied case.

## Components

### 1. `internal/netprompt/hostmap` (data + loader)

The JSON already exists. Add a Go loader:

```go
package hostmap

//go:embed opencode-egress.json
var raw []byte

type Map struct { /* host → Entry, built once */ }

func Load() (*Map, error)                       // parse embedded JSON
func (m *Map) Cause(host string) (string, bool) // exact + suffix match
```

- `Cause` lowercases, strips a trailing dot, tries exact then `match:"suffix"`.
- Returns the entry's `cause` string. `opt_in` and `not_egress` groups are ignored
  for lookup (telemetry has no fixed host; `not_egress` never dials).
- Parsed once at prompter construction; immutable thereafter.

### 2. `internal/netprompt/origin` (new package, behind an interface)

```go
package origin

type Origin struct {
    PID  int
    Name string // process basename only (e.g. "opencode", "curl") — NEVER full argv
}

type Resolver interface {
    // Resolve maps the child's source address (as seen by the proxy's
    // accepted conn.RemoteAddr) to the owning process. ok=false when it
    // can't be determined (unsupported OS, race, permission).
    Resolve(src netip.AddrPort) (Origin, bool)
}

func NewResolver() Resolver // Linux: procResolver; else: noopResolver{}
```

**Linux `procResolver`** (`resolver_linux.go`, build-tagged):

1. `src` is `127.0.0.1:<srcport>` from `conn.RemoteAddr()` (the child's socket).
2. Scan `/proc/net/tcp` (+ `tcp6`) for the row whose **local** address is
   `<srcport>` → read its socket **inode**.
3. Walk `/proc/*/fd/*`; the PID whose fd symlink is `socket:[<inode>]` owns it.
4. Read `/proc/<pid>/comm` for the process name (basename only).

**`noopResolver`** (`resolver_other.go`): `Resolve` always returns `ok=false`.

Design constraints:
- **Basename only, never `cmdline`.** `/proc/<pid>/cmdline` can contain secrets in
  argv (tokens, keys). We surface `comm` (e.g. `opencode`, `bun`, `curl`) — enough to
  answer "who," none of the risk. This is a hard rule, called out in tests.
- Best-effort: any error → `ok=false`, dialog omits the Origin line. Never blocks the
  prompt, never panics.
- The child is blocked awaiting the CONNECT response during the prompt, so it is
  alive — the lookup is reliable in practice; the race path is still handled.

### 3. Prompter enrichment (`internal/netprompt/prompt.go`)

- `Prompter` gains two optional fields: `hostMap *hostmap.Map`, `origin origin.Resolver`.
  Nil → that line is omitted (tests, unsupported OS).
- Replace the positional `Prompt(host, port)` with a small request struct so the
  source address can travel without a growing parameter list:

  ```go
  // in netproxy (the interface owner)
  type PromptRequest struct {
      Host   string
      Port   int
      Source netip.AddrPort // child's src addr; zero value = unknown
  }
  type Prompter interface { Prompt(PromptRequest) PromptResult }
  ```

- `promptText` gains the two lines, rendered above the existing intent line, each
  shown only when resolved:

  ```
  Origin:       <name>  (pid <n>)          // when origin.Resolve ok
  Likely cause: <cause>                     // when hostMap.Cause ok
  Agent intent: <reason | (not declared)>   // unchanged
  ```

- Ordering rationale: **who** and **what** first (the reassurance), then the agent's
  self-declared **why**. When intent *is* declared, all three can show — origin does
  not replace intent, it complements it.
- Backends (osascript/zenity/kdialog) need no interface change beyond passing the
  fuller text; they already render N-line bodies. `notificationText` stays short
  (banner) — origin/cause are popup-only, matching the intent spec.

### 4. Threading the source address (`internal/netproxy`)

The source addr exists at `server.go` `handleConnect` (`conn.RemoteAddr()`), but is
not currently threaded to the prompt. Thread it minimally:

- `filter.go`: `Check`/`CheckHost` gain a `src netip.AddrPort` param (zero value
  allowed); `defaultDecision` → `promptCoalesced` → `Prompter.Prompt` carry it into
  `PromptRequest`.
- `server.go`: `handleConnect`/`handleForward` capture `conn.RemoteAddr()` as
  `netip.AddrPort` and pass it through `admit`/`checkAndDial` into `Check`.
- Coalescing: when N requests for the same host share one in-flight prompt, the
  Origin shown is the leader's. Acceptable — documented; the cause line is
  host-derived and identical for all followers anyway.

This is the load-bearing ripple: the `Prompter` interface and `Check` signatures
change, touching their call sites and tests. Everything else is additive.

## Wiring (`internal/sandboxrun/run.go`)

At the existing `NewPrompter` call (`run.go:189`), also construct and inject the two
resolvers:

```go
hm, _ := hostmap.Load()          // nil-safe on error; cause line just won't show
res  := origin.NewResolver()     // noop off Linux
np, available := netprompt.NewPrompter(timeout, logf, lookupIntent, recordExplain, hm, res)
```

`hostmap.Load` failure is non-fatal (log + continue with nil map). `doctor.go:203`
constructs a prompter only to test backend availability — pass nils there.

## Security considerations

- **Advisory only.** Like intent, both lines are display text. They never enter the
  filter verdict, are never persisted, and cannot auto-grant. A spoofed process name
  cannot widen access — the user still chooses.
- **No secret leakage.** Basename (`comm`) only; `cmdline` is never read or shown.
- **Reader is the supervisor, same user.** Reading `/proc/<pid>` needs no elevation;
  the child runs as the same uid. PID-namespaced children still resolve because the
  socket inode is looked up in the host's `/proc` via the host PID.
- **Host map is static, embedded** — no runtime fetch, no injection surface.
- **Fail-open to less info, never to more access.** Every resolver failure removes a
  line; it never changes the decision or the default (`Deny once`).

## Error handling

- `hostmap.Load` error → nil map → cause line omitted; prompt still works.
- `origin.Resolve` error/unsupported → Origin line omitted.
- Zero-value `Source` (path not threaded, e.g. some test) → treated as unresolvable.
- `Cause` miss (host not in map) → cause line omitted (unknown host stays bare — the
  honest signal that omac has no attribution for it).

## Testing

1. `internal/netprompt/hostmap/hostmap_test.go` — `Load` parses the embedded JSON;
   `Cause` resolves a known host, exact + suffix; unknown host → `false`; `opt_in`
   and `not_egress` hosts are not matched.
2. `internal/netprompt/origin/resolver_linux_test.go` — parse a `/proc/net/tcp`
   fixture + a fake `/proc/<pid>` tree (inject root dir) → correct PID/name; assert
   **`cmdline` is never opened** (fixture omits it / fails if read); missing inode →
   `false`.
3. `resolver_other_test.go` — noop returns `false`.
4. `internal/netprompt/prompt_test.go` — extend: Origin + cause lines render when
   resolvers return values; each omitted independently when they don't; the existing
   intent-line cases still pass.
5. `internal/netproxy/filter_test.go` / `server_test.go` — update call sites for the
   new `src` param / `PromptRequest`; add a case asserting `Source` reaches the
   prompter (a fake prompter records the request).
6. No e2e change (parity with the intent spec; the prompt path is unit-covered).

## Out of scope / future

- **macOS origin** via `libproc`/`lsof` — follow-up; cause line already cross-platform.
- **Codex/copilot host maps** — same loader, additional data files, harness-keyed.
- **Exact URL** via opencode debug-log correlation — only if coarse cause is
  insufficient; brittle, deferred.
- **Suppressing the prompt entirely** for known-infra hosts — deliberately *not*
  chosen; this design keeps the user in the loop but makes the loop legible.
