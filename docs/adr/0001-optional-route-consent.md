---
title: "ADR 0001: Optional consent per sidecar route"
description: Introduction of an opt-in per-route skill approval mechanism and its configuration format.
---

Status: proposed. Tracked in [#455](https://github.com/TNG/oh-my-agentic-coder/issues/455), builds on [#428](https://github.com/TNG/oh-my-agentic-coder/issues/428).

## Context

A skill's sidecar runs on the host with the user's rights. Today the user approves the whole skill once, at `omac register`. After that, every call from the agent is forwarded. A skill with one dangerous operation next to harmless ones (say, "send mail" next to "list folders") cannot ask the user before just that one.

omac can show a dialog before a call is forwarded. The open question is what a skill has to declare so that omac knows when to show it.

## Options

**A. Closed world (the first design in #428).** The skill lists every route as `required` or `none`. A route that is not listed gets a 403.
Safe by default, but every skill author must list every route, and a route added to the sidecar later breaks the skill.

**B. Optional `approvals` block, unlisted routes ask (chosen).** A skill without the block behaves as today. A skill with the block asks for every route, except those it lists with `approval: none`.

**C. Rules based on the request body.** The skill says "ask when `source` is `mic`".
Needed for servers that put every operation behind one route (JSON-RPC, MCP). It needs body buffering and parsing, and is much more to specify. Not decided here.

## Decision

Option B:

- **No `approvals` block: nothing asks.** Existing skills must keep working without a change, so a missing block means today's behaviour.
- **With the block: a route asks unless an entry says `approval: none`.** An entry with `approval: required`, and a route with no entry, ask. A route added to the sidecar later must not skip consent just because the author forgot to list it.

Example: a mail skill lists `GET /folders` with `approval: none`. Reading folders never asks. `POST /send`, and any route added later, asks.

```yaml
sidecar:
  approvals:
    endpoints:
      - match: "GET /folders"
        approval: none
      - match: "POST /send"
        approval: required
        description: "Sends a mail"
```

The `POST /send` entry does not change whether that route asks. It gives the dialog a description, and an answer "always" for it covers every request the entry matches.

**Validation.** A block that breaks one of these rules stops `omac register`:

- Unknown keys in the block are rejected, and so is a misspelled block name such as `aprovals:`. A typo must not silently change what asks, and a misspelled block would otherwise read as no block at all.
- `approval: none` is not allowed on `ANY` or on a `**` path. Otherwise one entry such as `ANY /**: none` switches every dialog off, which is the same as having no block.
- Two entries of the same shape (same method, same layout of literal, `{name}` and `**` segments) are an error. Otherwise the order of the entries would decide which one applies.
- An empty `endpoints: []`, or an `approvals:` key with no value, is valid and means every route asks.

**Answers.** The user can at least answer once, for the session, or always. What else the dialog offers, its wording and its timeout are omac's choice. They are not part of the contract and can change in any release.

## Consequences

**Tradeoff: opt-in, and unlisted routes ask**
- Skill authors list only the routes that are safe to run unasked. A forgotten route asks too often, which is annoying but not dangerous.
- Adding the block to a skill changes every route it does not list: they all start asking.

**Unlisted routes and catch-alls are approved per exact path**
- An "always" answer for a declared entry covers everything the entry matches. For an unlisted route, and for an entry on `ANY` or a `**` path, it covers only that method and path: allowing `GET /messages/123` does not allow `GET /messages/124`, and allowing `GET` under `ANY /**` does not allow `DELETE`.
- Authors who want one answer for all ids declare `GET /messages/{id}`.

**Accepted risk: older omac ignores the block**
- An omac that predates this block forwards every call without a warning.
- Mitigation: do not publish a skill that relies on the block before an omac release that supports it. A file-wide format version is tracked in [#429](https://github.com/TNG/oh-my-agentic-coder/issues/429).

**Extension: body-based rules (option C)**
- If option C is added, it becomes a new key in an entry. Because unknown keys are rejected today, an omac without option C refuses such a skill at `omac register` instead of ignoring the rule.
