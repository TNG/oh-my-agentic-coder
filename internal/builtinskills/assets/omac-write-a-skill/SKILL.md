---
name: omac-write-a-skill
description: >-
  Author a new skill for the omac (oh-my-agentic-coder) execution shell — the
  agentskills.io SKILL.md plus omac's omac.yaml sidecar contract, the OMAC_*
  environment variables, the Unix-socket/loopback REST facade, secrets and
  mounts, install scripts, and the harness-agnostic rules. Use this when the
  user wants to create, scaffold, or package an omac skill (not a generic
  Claude/agentskills skill): "write an omac skill", "add a sidecar skill",
  "package this as an omac skill", "how do omac.yaml mounts/secrets work".
  Not for authoring non-omac skills.
license: Same as the omac repository
compatibility: >-
  Pure guidance skill — no sidecar, no omac.yaml, no network. Works unchanged
  under any omac inner harness (OpenCode, Claude Code, …). Shipped with the omac
  binary and provisioned by `omac setup`.
metadata:
  author: tngtech
  version: "0.1.0"
  omac-builtin: "true"
---

# omac-write-a-skill

Authoring helper for building a **skill that plugs into omac**
(`oh-my-agentic-coder`). This is omac-specific: it covers omac's runtime
contract — the `omac.yaml` sidecar block, the `OMAC_*` env vars, the
Unix-socket + loopback-TCP REST facade, secrets/config/mounts, install
scripts, and the harness-agnostic rules — not generic agentskills or
Claude-skill authoring.

> Reach for this skill only when authoring an **omac** skill. For unrelated
> "write a skill" requests, a generic authoring skill is the better fit.

## How to use it

The complete, authoritative guide is bundled alongside this file:

**→ `references/creating-a-skill.md`** — read it before authoring.

It walks through, in order:

1. Why sidecars exist and how the sandbox boundary works.
2. On-disk skill layout and naming rules.
3. The `SKILL.md` format (agentskills.io discovery file).
4. The `omac.yaml` schema (omac's runtime contract).
5. Writing the HTTP sidecar (stdlib-only Python reference).
6. Install scripts, route URL rewriting, and `OMAC_*` env wiring.
7. Secrets vs. non-secret config, and best practices.
8. Local development, testing, and the pre-shipping checklist.

## Workflow

1. Read `references/creating-a-skill.md` end to end (or jump to the section the
   task needs — the headings map to the list above).
2. Scaffold the skill directory with `SKILL.md` + `omac.yaml` (+ `scripts/`,
   `install/` as needed), following the layout and schema in the guide.
3. Implement the sidecar against the omac facade contract; keep it
   harness-agnostic (only `OMAC_*` + REST, never a harness-specific path).
4. Validate with `omac register <skill>` and a local round-trip before shipping.
