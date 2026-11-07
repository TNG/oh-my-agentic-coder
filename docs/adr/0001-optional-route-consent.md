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
Safe by default, but every skill author must list every route, and a route added to the sidecar later breaks the skill.

**B. Optional `approval` block, unlisted routes ask (chosen).** A skill without the block behaves as today. A skill with the block asks for every route, except those it lists with `approval: none`.
Old skills keep working without a change. A route added to the sidecar later is asked about until the author says otherwise, so forgetting one is safe.

**C. Rules based on the request body.** The skill says "ask when `source` is `mic`".
Needed for servers that put every operation behind one route (JSON-RPC, MCP). It needs body buffering and parsing, and is much more to specify. Not decided here.

## Decision

Option B. The block is optional, and so is the field on each entry:

- No `approvals` block: routes are forwarded without a dialog, as before.
- With the block: a route asks unless an entry for it says `approval: none`. An entry with `approval: required`, and a route with no entry, ask.

The user chooses how long an answer holds: once, for the session, or permanently.

omac decides what the dialog shows and what else the user can do in it. That is not part of the contract and can change in any release.

Example: a mail skill lists `GET /folders` with `approval: none` and nothing else. Reading folders never asks. `POST /send`, and any route added later, asks.

## Consequences

- Skill authors list only the routes that are safe to run unasked, with `approval: none`. A forgotten route asks too much, which is annoying but not dangerous.
- An omac that predates this field ignores it and forwards everything, without a warning. The block therefore keeps its own `version`, and a skill that depends on it should not be published before a release that has it. A file-wide format version is tracked in [#429](https://github.com/TNG/oh-my-agentic-coder/issues/429).
- Content-based rules (option C) can be added later as a new key in the same entry, behind a new `version`. A key that is reserved today is rejected, so no skill can use it by accident.
- The dialog, the verdict store, path cleaning and the audit trail from #428 are reused unchanged.
- A skill that adds the block asks about every route it does not list. Adding one entry therefore changes the behaviour of all the others, and the docs say so.
