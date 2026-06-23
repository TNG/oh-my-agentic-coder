## Context

omac distinguishes two kinds of skills (`internal/skillsource`): a directory with an
`omac.yaml` is an **omac sidecar skill** (omac discovers, registers, and activates it);
a directory with only a `SKILL.md` "does not have an omac sidecar contract, so omac
ignores it (no registration, no spawning)" — it is a **guidance-only skill** surfaced
directly by the *harness's own loader* from its native skills dir
(`~/.config/opencode/skills`, `~/.claude/skills`). The `openspec-*` and `caveman`
skills are guidance-only; `skill-marketplace` is a sidecar skill.

The authoring helper exists today only as `CREATING_A_SKILL.md` in the repo root — not
as a shipped skill, and not on disk in any harness skills dir. Populating those dirs
was `opencode-nono`'s `install.sh`, now retired. omac embeds only
`internal/plugin/assets/omac-multidir.ts`; no skills are bundled.

`omac-write-a-skill` exposes no API — it is pure authoring guidance — so it is a
guidance-only skill. That means provisioning is **file placement only**: omac neither
registers nor activates it, and it must land in each harness's *native* skills dir
(not the shared `~/.config/agents/skills`, which omac scans but the harnesses do not
read).

## Goals / Non-Goals

**Goals:**
- Ship a guidance-only `omac-write-a-skill` bundle inside the omac binary.
- Provide `omac setup` to write it into each installed harness's native skills dir,
  idempotently — replacing the dead `install.sh` path.
- Keep it omac-specific and collision-proof against third-party authoring skills.
- Add a Quickstart `omac setup` step so a fresh install works out of the box.

**Non-Goals:**
- Any registry / skill-config / keychain / `serve` activation work — guidance skills
  bypass all of it.
- Anything marketplace-related (stage, base URL, the `skill-marketplace` bundle).
- A general plugin/skill package manager — only this one built-in bundle.
- Giving the skill an `omac.yaml`/sidecar.

## Decisions

### D1 — Embed the bundle with `go:embed` (a new `internal/builtinskills` package)
Bundle the skill as a directory tree under
`internal/builtinskills/assets/omac-write-a-skill/` and expose it via `embed.FS`,
mirroring how `internal/plugin` embeds `omac-multidir.ts`, so it versions with omac.
- *Alternative:* fetch from a release artifact at provision time → rejected: needs
  network for something that should be self-contained and offline.

### D2 — Auto-provision on launch; `omac setup` for explicit (re)provisioning
Provisioning is folded into `omac start`/`omac serve`: on launch omac idempotently
materializes the embedded bundle into the **active** harness's native skills dir, so
there is **no extra setup step**. It is silent when already current and never fails the
launch (a write error or a foreign same-named dir is a warning). No registration: omac
ignores `SKILL.md`-only dirs, and the harness surfaces the skill itself.

An explicit `omac setup [harness] [--force]` command remains for provisioning **all**
installed harnesses at once and for force-refreshing after an upgrade — but the
everyday flow needs no separate command.
- *Decision rationale:* the user prioritized a minimal, single-step install. Auto-on-
  launch achieves that; the cost is that `start`/`serve` write into the harness's
  global skills dir as a side effect (idempotent, marker-guarded, scoped to the active
  harness only).
- *Alternatives:* an explicit-only `omac setup` step (rejected: adds a step to the
  Quickstart); provision-on-first-run-only (rejected: needs extra "first run" state);
  fold into `omac register` (rejected: register is for sidecar skills and requires an
  `omac.yaml`).

### D3 — Provision into each installed harness's native skills dir (all harnesses)
A guidance skill is surfaced by the harness's own loader, which reads only its own
native global skills dir — `~/.config/opencode/skills` for OpenCode (XDG honored),
`~/.claude/skills` for Claude Code. The shared `~/.config/agents/skills` is an omac
discovery convention the harnesses do not read natively, so it would NOT surface a
guidance skill. Auto-provisioning on launch targets the **active** harness's native
dir; the explicit `omac setup` writes a copy into **each installed harness's** native
dir (detected via the harness's inner command on `PATH`), matching what `install.sh`
did. Both use a per-harness "native global skills dir" resolver in `internal/config`
(Claude Code's config home is `~/.claude`, not `~/.config/claude`).
- *Alternative:* a single shared-dir copy (rejected: harnesses don't read it
  natively); active-harness-only (rejected per product decision — want it everywhere).

### D4 — Guidance-only bundle sourced from `CREATING_A_SKILL.md`, drift-guarded
The bundle is a `SKILL.md` (frontmatter + concise overview) plus
`references/creating-a-skill.md`, a byte-exact copy of the repo-root
`CREATING_A_SKILL.md`. `go:embed` cannot reach above the package dir, so the copy
lives in the bundle; a test asserts the two are byte-identical, keeping the repo root
as the single source of truth.

### D5 — omac-namespaced name + ownership marker (collision-proofing)
omac/harness discovery key skills by **name**, and popular authoring skills already
exist in global dirs (superpowers' `writing-skills`, Anthropic's `skill-creator`). To
stay omac-specific and never clash:
- The bundle's directory and SKILL.md `name` are **`omac-write-a-skill`**.
- The SKILL.md `description` is scoped to *authoring omac skills* (the `SKILL.md` +
  `omac.yaml` sidecar contract, `OMAC_*` env, REST facade), so the harness selects it
  only for omac authoring.
- Ownership is machine-readable: `metadata.author: tngtech` plus an
  `metadata.omac-builtin: true` marker.
- `omac setup` overwrites only a directory carrying the `omac-builtin` marker; a
  foreign same-named directory is left untouched and the user is warned.
- *Alternative:* a generic name (`write-a-skill` / `writing-skills`) — rejected: it
  would shadow or be shadowed by popular skills, and reads as unowned.

## Risks / Trade-offs

- **R1: Claude Code's native global skills dir is nonstandard** (`~/.claude`, not
  `~/.config/claude`). → The per-harness resolver (D3) encodes this explicitly and is
  unit-tested; provisioning targets the dir each harness actually reads (verified
  against where `skill-marketplace` already lives).
- **R2: overwriting on re-run could clobber local edits**, or a foreign same-named
  skill. → `omac setup` overwrites only a directory carrying the `omac-builtin` marker
  (D5), refreshing omac-owned files and reporting what changed; a foreign same-named
  directory is left untouched with a warning.
- **R3: omac-write-a-skill / CREATING_A_SKILL.md drift.** → Byte-exact copy + a test
  asserting equality (D4).

## Open Questions

- None outstanding. (Resolved: auto-provision on launch for a single-step install;
  `omac setup` retained for all-harness/forced refresh; `omac doctor` reports per-
  harness state.)
