# Intent declarations for sandbox prompts

**Issue:** [#41 — Better descriptions for web requests](https://github.com/TNG/oh-my-agentic-coder/issues/41)
**Date:** 2026-07-07
**Status:** Approved, awaiting implementation plan

## Problem

When the omac sandbox intercepts a request to a new external host (or a
protected folder), the user-facing popup shows only `host:port`. The user
approves or denies blind — they have no idea *why* the agent wants to reach
that host or what it expects to find there. Issue #41 asks for a better
description; the issue commenter clarified: "when the agent requests access
to a new web address."

The same gap exists for folder access, where the only signal the user gets
is a path at session-end learn-mode review (or an EACCES/marker file at
access time, which never reaches the user directly).

## Goal

Surface the agent's **intent** — why it needs access and what it promises
to find — in every access prompt:

- Network popup: show the URL path (when available) and the agent-declared
  reason for the request.
- Folder access: no live popup (out of scope for this PR — see
  *Non-goals*), but the agent is briefed to pre-declare intent for paths
  outside granted dirs, and the declared intent surfaces in the
  session-end learn-mode review alongside each candidate folder.
- "Explain more" button: when the pre-declared intent is missing or
  insufficient, the user can click "Explain more" — the request is denied
  with a marker telling the agent to declare or refine its intent, then the
  agent retries.

The agent is *briefed* to pre-declare; it is not forced to. A request
with no intent on file still produces a popup — it just says "(not
declared)" and offers "Explain more".

## Non-goals

- **Live folder popup.** A kernel-level filesystem prompter (pause the
  process on EACCES, show a popup, resume on allow) is a much larger
  surface — new prompter package, macOS Seatbelt + Linux Landlock
  interception hooks. Deferred to a follow-up issue. Folder intent in this
  PR is collected via the same `/sandbox/intent` endpoint and surfaced only
  at session-end learn-mode review.
- **Forcing intent.** The agent is briefed but not required. Enforcement
  (auto-deny with no intent) was considered and rejected — it adds a
  round-trip on every new host and breaks first-contact SSE/long-poll.
- **Notifications with intent.** The OS notification banner text stays
  short; intent is too long for a banner. Only the popup shows it.
- **E2E test changes.** The existing echo-rest e2e doesn't exercise the
  network prompt path; that's covered by unit tests. A prompt-driven e2e
  variant is YAGNI for this PR.

## Architecture

One new in-supervisor component (intent registry) and five surface
changes (facade endpoint, brief, network popup, folder deny text,
learn-mode review).

```
                    ┌─────────────────────────────────────────────┐
                    │              Supervisor process             │
                    │                                             │
   agent ─POST──►  │  facade  ──►  intent.Registry  ◄──lookup──  netproxy.Filter
   /sandbox/intent │                                              │
                    │                            └──lookup──►  netprompt.Prompter
                    │                                              │
                    │  learnRecorder ──lookup──►  intent.Registry │
                    └─────────────────────────────────────────────┘
```

All components live in the same process; the registry is an in-memory
map, never persisted. The agent only talks to the facade endpoint; the
popup reads the registry directly.

## Components

### 1. `internal/intent` (new package)

**`registry.go`:**

```go
package intent

type Entry struct {
    Target string    // host (lowercased) or absolute path (cleaned)
    Reason string
    Time   time.Time
}

type Registry struct {
    mu      sync.Mutex
    entries map[string]Entry
    ttl     time.Duration
    logf    func(string, ...any)
}
```

- `New(ttl time.Duration, logf func(string, ...any)) *Registry` — default
  ttl 10 minutes (caller passes this; tests use a short ttl).
- `Record(target, reason string)` — normalizes the target:
  - For network targets (no path separator, parseable as host): lowercase.
  - For path targets: `filepath.Clean` + `filepath.Abs`.
  - Empty reason → no-op (logged at debug).
  - Overwrites any prior entry for the same target.
- `Lookup(target string) (Entry, bool)` — exact match after the same
  normalization. Returns false if expired (entry is lazily deleted).
- `LookupHost(host string) (Entry, bool)` — lowercases host, delegates to
  `Lookup`. Convenience for the netproxy layer.
- `Record` is a no-op when the registry is nil (tests, `--learn` without
  facade).
- Background goroutine sweeps expired entries every `ttl/2`. Started by
  `New`; stopped by a `Close()` method called at session end.

No persistence, no HTTP — the registry is process-local. The only
external surface is the facade endpoint (below).

### 2. Facade endpoint

**`internal/facade/facade.go`** — new route `POST /sandbox/intent` next
to the existing `/sandbox/denied` route.

- Request body: `{"target":"<host or absolute path>","reason":"<one sentence>"}`
- Auth: same `OMAC_TOKEN` bearer as other facade endpoints.
- On success: records to the injected `*intent.Registry`, returns `204 No
  Content`.
- On empty/missing `target` or `reason`: `400` with a short body naming
  the required fields.
- Registry injected at facade construction time (same pattern as
  `deniedChecker` today). Nil registry → endpoint returns `503` with a
  "intent registry not available" body (defensive; should not happen in
  production wiring).

No GET endpoint. The popup reads the registry in-process; the agent
never needs to read intents back.

### 3. Brief update

**`internal/sandboxbrief/brief.md`** — two changes:

1. Extend the existing **Network** bullet with one sentence:

   > If you declared an intent, the user sees it; if not, the dialog says
   > so and offers an "Explain more" button that denies and asks you to
   > elaborate.

2. New bullet after **Capabilities**:

   - **Intent:** before contacting a new external host, or before
     accessing a path outside your granted directories, declare why:
     `POST $OMAC_BASE/sandbox/intent` with JSON
     `{"target":"<host or absolute path>","reason":"<one sentence: why
     you need it and what you expect to find>"}`. The user sees your
     reason in the approval popup (network) or the session-end review
     (folders). A request with no intent on file still works — it just
     pops up a less informative dialog, and the user can click
     "Explain more" to send you back for a reason before deciding.

### 4. Network popup enrichment

**`internal/netprompt/prompt.go`:**

- `Prompter` gains a field `lookupIntent func(host string) (string, bool)`.
  Wired in `NewPrompter` via an extra parameter from `run.go`. Nil → no
  registry (tests); all lookups return false.
- `promptText(host, port, urlPath, intent string)` — new signature:

  ```
  The sandboxed process is trying to reach:

      https://example.com:443/some/path

  Agent intent: "fetch the latest release notes to verify the version"
  ```

  - URL path shown when known. CONNECT (HTTPS) can't see the path — only
    `host:port`. Forward HTTP can. When unknown, show `host:port` as today.
  - Intent line shown when the lookup returns a reason. Otherwise:
    `Agent intent: (not declared)`.
- `optionLabels` gains a seventh entry: `"Explain more"`. Maps to new
  token `tokenNeedsIntent`.
- Default selection stays `"Deny once"` (not "Explain more") — Explain
  more is an explicit choice.

**`internal/netproxy/filter.go`:**

- `PromptResult` gains `NeedsIntent bool`.
- `defaultDecision` sets the verdict reason to `"prompt:needs_intent"`
  instead of `"prompt:deny"` when `NeedsIntent` is true. Persistence
  logic unchanged (NeedsIntent is never persisted — it's a one-shot
  signal to the deny body).

**`internal/netproxy/server.go`:**

- `denyBody` branches on reason: when reason contains `needs_intent`, the
  body tells the agent to declare or refine its intent and retry:

  ```
  omac sandbox: access to "example.com" was DENIED — the user asked for
  more explanation.

  Declare or refine your intent via:
    POST $OMAC_BASE/sandbox/intent  {"target":"example.com","reason":"..."}
  then retry the request.
  ```

- Uses the literal `$OMAC_BASE` token (the brief defines what it resolves
  to; same convention as the existing deny body referencing profile paths).
- The three backends (osascript, zenity, kdialog) each gain the seventh
  radio option. No new dependencies; the dialogs already support N
  options.

### 5. Folder deny text + learn-mode enrichment

**`internal/sandboxdeny/deny.go`** — `DenialText` (the marker file
content on Linux bwrap; the EACCES-adjacent message on macOS) gains a
trailing hint:

```
If you need this path for your task, declare why first:
  POST $OMAC_BASE/sandbox/intent  {"target":"<absolute path>","reason":"..."}
The user will see your reason when reviewing access.
```

Added unconditionally — costs nothing, works whether or not the agent
ever calls the endpoint. The agent learns the convention from the deny
itself, not only the brief. The bwrap `DenialText` in `grants.go` flows
this through unchanged (it already plumbs `resolvedDenialText` into the
marker).

**`internal/sandboxrun/learn.go`:**

- `learnRecorder` gains a reference to the `*intent.Registry` (passed via
  `newLearnRecorder(g, reg)`; nil in tests that don't care).
- During `candidates()` aggregation, for each candidate path, call
  `reg.Lookup(path)`. Stash the reason alongside the path in a new
  internal struct `{Path, Intent string}`.
- `OfferLearnedFolders` prints the intent next to each candidate:

  ```
  omac sandbox: learn mode observed these folders outside the current profile:
    ~/some/dir        — agent said: "read project config"
    ~/other/dir       — agent said: "load fixture data"
    ~/no-intent-dir   — (no intent declared)
  ```

- When intent is absent, print `(no intent declared)` so the contrast is
  visible.
- Intent is informational only — it does not auto-grant. User still
  answers `[y/N]` as today.
- In restricted (non-learn) sessions, intents are recorded but never
  shown (no learn-mode review). No harm; the registry just expires.

## Wiring

**`internal/sandboxrun/run.go`** constructs one `*intent.Registry` per
session and threads it through:

1. Facade construction (for `POST /sandbox/intent` recording).
2. `NewPrompter` (for `lookupIntent` in the popup).
3. `newLearnRecorder` (for candidate intent lookup at session end).

Registry lifetime = session. `Close()` called on shutdown.

## Error handling

- `POST /sandbox/intent` with missing/empty `target` or `reason` → `400`
  with a short body naming the required fields.
- Registry full? No cap — entries are small and TTL-evicted; a
  misbehaving agent spamming intents just churns the map. Logged at
  debug level via the injected `logf`.
- `Lookup` on a missing key returns `("", false)` — callers already
  handle this (popup shows "not declared").
- Registry nil (tests, `--learn` without facade) → all lookups return
  false, no panic. `Record` is a no-op.
- `NeedsIntent` is never persisted (no permanent "explain more" rule
  makes sense). It's a one-shot signal from the popup to the deny body.

## Testing

1. `internal/intent/registry_test.go` — `TestRegistryRecordLookup`,
   `TestRegistryTTLExpiry`, `TestRegistryNilSafe`,
   `TestRegistryPathNormalization`, `TestRegistryHostLowercase`.
2. `internal/netprompt/prompt_test.go` — extend: assert intent line
   appears in `promptText` when present, `(not declared)` when absent;
   assert `needs_intent` token round-trips through `labelToToken` /
   `tokenToResult`; assert `PromptResult.NeedsIntent` is set.
3. `internal/netproxy/filter_test.go` — `TestFilterNeedsIntentVerdictReason`:
   prompter returns `PromptResult{NeedsIntent:true}`; assert verdict
   reason is `prompt:needs_intent`.
4. `internal/netproxy/server_test.go` — `TestServerNeedsIntentDenyBody`:
   assert the deny body mentions `/sandbox/intent` when reason is
   `needs_intent`.
5. `internal/facade` — `TestFacadeIntentEndpoint`: `POST /sandbox/intent`
   records; `400` on empty body; `503` when registry is nil.
6. `internal/sandboxrun/learn_test.go` — extend `testRecorder` to take a
   registry; assert `OfferLearnedFolders` prints intent lines and
   `(no intent declared)` for missing intents.
7. `internal/sandboxdeny/deny_test.go` — assert deny text mentions
   `/sandbox/intent`.

No e2e changes (see Non-goals).

## Out of scope / future

- **Live folder popup** — kernel-level fs prompter (pause on EACCES,
  show popup, resume). Follow-up issue.
- **Forced intent** — auto-deny when no intent on file. Rejected for this
  PR; could be a profile knob later (`network.network_prompt.require_intent`).
- **Intent persistence** — intents are session-scoped; a future
  `intent_history` could feed provenance. YAGNI now.
- **Prompt-driven e2e** — a variant of echo-rest that exercises the
  network prompt + intent flow end-to-end. Add when the prompt path
  stabilizes.
