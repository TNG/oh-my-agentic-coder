---
title: "ADR 0001: Optional consent per sidecar route"
description: Why a skill marks the routes that need the user's consent, and nothing more
---

Status: proposed. Tracked in [#455](https://github.com/TNG/oh-my-agentic-coder/issues/455), builds on [#428](https://github.com/TNG/oh-my-agentic-coder/issues/428).

## Context

A skill's sidecar runs on the host with the user's rights. Today the user approves the whole skill once, at `omac register`. After that, every call from the agent is forwarded. A skill with one dangerous operation next to harmless ones (say, "send mail" next to "list folders") cannot ask the user before just that one.

omac can show a dialog before a call is forwarded. The open question is what a skill has to declare so that omac knows when to show it.

## Options

**A. Closed world (the first design in #428).** The skill lists every route as `required` or `none`. A route that is not listed gets a 403.
Safe by default, but every skill author must list every route, and a route added to the sidecar later breaks the skill. Old skills would need a rewrite to benefit.

**B. Optional `ask` per route (chosen).** The skill marks only the routes that need consent. Everything else behaves as today.
Small to write, and old skills keep working without a change. The cost is that a route the author forgot to mark is not protected.

**C. Rules based on the request body.** The skill says "ask when `source` is `mic`".
Needed for servers that put every operation behind one route (JSON-RPC, MCP). It needs body buffering and parsing, and is much more to specify. Not decided here.

## Decision

Option B. A route in `omac.yaml` may carry `ask: true`. Without it, and for skills without the block, a route is forwarded as before. The user chooses how long an answer holds: once, for the session, or permanently.

omac decides what the dialog shows and what else the user can do in it. That is not part of the contract and can change in any release.

Example: a mail skill marks `POST /send` with `ask: true`. `GET /folders` has no entry and is never asked about.

## Consequences

- Skill authors write a few lines, not a full route table. Forgetting to mark a dangerous route leaves it unprotected, so authors must review their routes.
- An omac that predates this field ignores it and forwards everything, without a warning. The block therefore keeps its own `version`, and a skill that depends on it should not be published before a release that has it. A file-wide format version is tracked in [#429](https://github.com/TNG/oh-my-agentic-coder/issues/429).
- Content-based rules (option C) can be added later as a new key in the same entry, behind a new `version`. A key that is reserved today is rejected, so no skill can use it by accident.
- The dialog, the verdict store, path cleaning and the audit trail from #428 are reused unchanged.
- A skill can later become stricter (a closed world as an opt-in) without breaking this contract.
