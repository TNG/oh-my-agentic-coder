# Verifying intent declarations: reliability & performance

**Related:** PR #51 (`feat(sandbox): intent declarations`), issue #41
**Date:** 2026-07-15
**Status:** Investigation / proposal

## Why this needs its own note

The intent-declarations feature has two very different kinds of correctness,
and the existing tests only cover one of them:

1. **Mechanism** — the registry stores and normalizes targets, the facade
   endpoints answer, the popup renders the reason, the deny bodies carry the
   hint. This is deterministic and is covered by unit + integration tests
   (`internal/intent`, `internal/facade`, `internal/netprompt`,
   `internal/sandboxdeny`) and by the round-trip e2e `TestE2EIntentPrompt`.

2. **Behavior** — does an agent, given only the sandbox brief, *actually
   pre-declare* its intent before hitting a new host or a path outside its
   grants? And when denied, does it use the reactive channel
   (`GET /sandbox/intent`) and follow the `hint`? **This is the feature's
   whole value proposition, and nothing currently measures it.**

The gap is concrete: `internal/e2e/intent_test.go` **scripts** the
declaration — the prompt literally instructs the agent to
`curl -X POST .../sandbox/intent`. That proves the plumbing works end to
end; it says nothing about whether the agent would declare on its own. The
behavior depends on LLM cognition (the brief wording, the model, the
harness), not on a code path, so it can only be established by *measuring a
rate over repeated real runs*, never by a single assertion.

This note focuses on behavior (the primary interest) and treats performance
as a secondary, already-mostly-handled axis.

## Part 1 — Behavioral reliability (the primary question)

### What to measure

| Metric | Definition | Why it matters |
| --- | --- | --- |
| **Pre-declaration rate** | Of the network prompts / protected-path touches a run triggers, the fraction for which a *matching* intent was already on file. | The headline number: how often the user actually gets a reason. |
| **Reactive-recovery rate** | After a denial with no prior intent, the fraction of cases where the agent queries `GET /sandbox/intent` and then follows the `hint` (declare + retry, or stop). | Measures the fallback channel the brief promises works. |
| **Intent quality** | Does the declared `reason` actually describe the request it precedes? (LLM-judge, 1–5.) | A declared-but-useless reason ("accessing the network") passes the rate metric but fails the user. |
| **Target-match rate** | Of declared intents, the fraction whose `target` normalizes to the host/path that actually prompted. | Catches the agent declaring `https://api.x/v2` while the request pins `api.x` — a normalization or agent-phrasing miss that silently shows "(not declared)". |

Pre-declaration rate is the one to move first; the others explain *why* it is
what it is.

### The enabler shipped in this PR

Behavior is only measurable if each prompt records whether intent was
present. PR #51 now emits one machine-parseable line per network prompt
(`internal/netprompt/prompt.go`):

```
omac sandbox: intent-signal: network prompt host=<h> port=<p> intent=declared|missing
```

This turns pre-declaration rate into a `grep | count` over any session's
diag log — no eval harness required to get a first read, and it is the
assertion surface a behavioral e2e keys off. (Guarded by
`TestPromptEmitsIntentSignal`.) A parallel signal for the folder path can be
added at the learn-review lookup if folder behavior needs the same
treatment.

### How to measure it properly: a brief-only behavioral e2e

The decisive test is a sibling of `TestE2EIntentPrompt` that **removes the
scripted POST**. Shape:

1. Give the agent a realistic task that *requires* reaching a host not in
   `allow_domain` (e.g. "fetch and summarize the changelog at
   `https://<stub-host>/changelog`") — but do **not** mention intent, the
   endpoint, or declaring. Only the brief is in play.
2. Drive it with the stub prompter (`OMAC_PROMPT_STUB=1`) so no human is
   needed and the run is non-interactive.
3. Assert on *observed behavior*, not on scripted steps:
   - the registry received a matching intent **before** the prompt fired
     (inspect via `GET /sandbox/intent?target=<host>` post-run, or parse the
     `intent-signal:` line), and
   - the reason is non-trivial (length / LLM-judge gate).
4. A single run is a coin flip, not a result. Run **N times** (e.g. 5–10)
   and report the *rate*; treat the test as a threshold check
   (e.g. "≥ 70% pre-declared") rather than a boolean.

### Where it runs — not a PR gate

Each run drives a real LLM and costs real tokens, and the outcome is
stochastic, so this belongs on a **cadence lane**, not the blocking PR
pipeline:

- Run across the **harness matrix** (opencode / claude-code / codex /
  copilot) and, where feasible, **more than one model** — declaration
  behavior is model- and harness-specific, and a single-model pass hides
  regressions elsewhere.
- Schedule it like the existing weekend/agent-driven lanes rather than per
  PR; publish the per-harness rate as a tracked metric so a brief edit that
  *lowers* the declaration rate is caught.
- The deterministic mechanism tests stay the PR gate; the behavioral rate is
  signal for a human, the same split the repo already uses for its
  agent-driven suites.

### Feeding results back

The brief is the main lever on this rate. When a run shows a low
pre-declaration or target-match rate, the fix is usually a brief edit
(clearer "declare *before* the first request", a worked example, tightening
the network-vs-folder contrast — note the brief was just de-duplicated in
this PR). Re-running the lane after a brief change is how you tell an
improvement from a plausible-sounding rewrite.

## Part 2 — Performance (secondary, largely addressed here)

Performance of the intent path is deterministic and cheap to pin down; the
review already drove most of it, and PR #51 tightened the hot paths:

- **Popup latency.** The exact lookup on the popup path is now bounded to
  `popupLookupTimeout` (500 ms) via a per-call context, and uses a shared
  keep-alive `http.Client` (`internal/intent/lookup.go`). A wedged facade no
  longer delays the popup by up to 2 s; a healthy loopback lookup is
  sub-millisecond.
- **Teardown latency.** The learn-review folder lookups now run with bounded
  concurrency (`intentLookupParallelism = 4`,
  `internal/sandboxrun/learn.go`) instead of `2s × N` serial stalls.
- **Registry cost.** In-memory, mutex-guarded, O(1) per op except
  `evictOldest`/`LookupSubtree` which are O(n) at `n ≤ 512` — negligible.

What is still worth adding (small, deterministic, CI-friendly) are Go
microbenchmarks so any future regression is caught mechanically:

- `BenchmarkRegistryRecord`, `BenchmarkRegistryLookup`,
  `BenchmarkRegistryLookupSubtree` (populated to the 512 cap),
  `BenchmarkEvictOldest`.
- A latency check for `LookupOverHTTP` against a live facade (reuse the
  `lookup_integration_test.go` harness) asserting p99 well under the 500 ms
  popup budget.

These are left as a follow-up: they guard numbers that are currently
comfortable, whereas the behavioral lane above guards the property that is
currently *unmeasured*.

## Summary

- The unmeasured risk is **behavioral**, not mechanical. Prioritize the
  brief-only behavioral e2e + rate metrics over more mechanism tests.
- This PR ships the **observability enabler** (`intent-signal:` diag line)
  that makes the pre-declaration rate computable from any session and
  assertable by that e2e.
- Run behavioral measurement on a **cadence lane across harnesses/models**,
  not the PR gate; keep the deterministic mechanism tests as the gate.
- Performance is deterministic and mostly handled by this PR's fixes;
  microbenchmarks are a cheap follow-up.
