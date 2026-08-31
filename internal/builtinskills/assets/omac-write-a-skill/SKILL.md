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
  Pure guidance skill: no sidecar and no omac.yaml. It links to the authoring
  guide on GitHub, so reading that guide needs network access to github.com.
  Works under any omac inner harness. Shipped with the omac binary and
  provisioned by `omac setup`.
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

The complete, authoritative guide is the omac skill-authoring doc:

**→ https://raw.githubusercontent.com/TNG/oh-my-agentic-coder/main/docs/skills/authoring.md**

Fetch and read it before authoring.

## Workflow

1. Fetch and read the guide above.
2. Scaffold the skill directory with `SKILL.md` + `omac.yaml` (+ `scripts/`,
   `install/` as needed), following the layout and schema in the guide.
3. Implement the sidecar against the omac facade contract; keep it
   harness-agnostic (only `OMAC_*` + REST, never a harness-specific path).
4. Validate with `omac register <skill>` and a local round-trip before shipping.
