# Codex + Copilot Backends — Remaining Work Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete the codex + copilot backend support: openspec change proposal, README/docs updates, and the deferred manifest migration.

**Architecture:** The core Go implementation (harness descriptors, session listing, manifest generator, bridge scripts) is already TDD-complete and committed. This plan covers the remaining documentation and openspec artifacts.

**Tech Stack:** Go, bash, markdown, openspec

---

## Already Complete (committed)

- `internal/config/harness.go` — codex + copilot descriptors, `SessionListCodex`/`SessionListCopilot` enum values
- `internal/session/session.go` — `listCodex()`, `listCopilot()`, dispatch + best-effort listing
- `internal/manifest/manifest.go` — `Render()` function (extraction, tested)
- `.codex/hooks/omac-bridge.sh` + `.codex/hooks.json` — codex bridge (SessionStart only)
- `.copilot/hooks/omac-bridge.sh` + `.copilot/hooks/omac.json` — copilot bridge (SessionStart + SessionEnd)
- `docs/superpowers/specs/2026-06-29-codex-copilot-backends-design.md` — design spec
- All tests pass (`go test ./...`)

---

### Task 1: Create openspec change proposal

**Files:**
- Create: `openspec/changes/support-codex-copilot-harnesses/proposal.md`
- Create: `openspec/changes/support-codex-copilot-harnesses/tasks.md`

- [ ] **Step 1: Read the claude-code-harness proposal as template**

Read `openspec/changes/support-claude-code-harness/proposal.md` and `openspec/changes/support-claude-code-harness/tasks.md` to understand the format.

- [ ] **Step 2: Write the proposal**

Create `openspec/changes/support-codex-copilot-harnesses/proposal.md`:

```markdown
# Support Codex + Copilot CLI Harnesses

## Summary

Add Codex CLI (`codex`) and GitHub Copilot CLI (`copilot`) as supported
harnesses in omac, following the same declarative pattern used for OpenCode
and Claude Code.

## Motivation

omac's harness abstraction is designed to be extensible. Adding codex and
copilot brings the total to four supported agentic coders, each with full
skills manifest injection, session continue/resume, and facade access.

## Design

See `docs/superpowers/specs/2026-06-29-codex-copilot-backends-design.md` for
the full design. Key decisions:

- Two new `Harness` descriptors in `harnessRegistry()`
- Two new `SessionListKind` enum values
- Native bridge scripts (shell, not MCP) per harness
- Codex: SessionStart only (Stop is turn-scoped, no deactivate)
- Copilot: SessionStart + SessionEnd, user-level hooks (not .github/hooks/)
- Shared manifest generator extracted to `internal/manifest/`
- Copilot alias is `co` (not `gh-copilot` — exec is standalone `copilot`)

## Scope

In scope:
- Harness descriptors + session listing
- Bridge scripts + hook registration
- Manifest generator extraction

Deferred to separate change:
- `omac manifest` subcommand + migration of existing bridges
- README.md + CREATING_A_SKILL.md updates
- `COPILOT_HOME` env override support
- Pre-flight binary check (`exec.LookPath`)
```

- [ ] **Step 3: Write the tasks file**

Create `openspec/changes/support-codex-copilot-harnesses/tasks.md`:

```markdown
# Tasks

- [x] Add codex + copilot Harness descriptors to `harnessRegistry()`
- [x] Add `SessionListCodex` and `SessionListCopilot` enum values
- [x] Add `listCodex()` and `listCopilot()` session listing functions
- [x] Extract shared manifest generator to `internal/manifest/Render()`
- [x] Create codex bridge script (`.codex/hooks/omac-bridge.sh`)
- [x] Create codex hooks registration (`.codex/hooks.json`)
- [x] Create copilot bridge script (`.copilot/hooks/omac-bridge.sh`)
- [x] Create copilot hooks registration (`.copilot/hooks/omac.json`)
- [x] Write design spec
- [ ] Create openspec change proposal
- [ ] Update README.md harness list
- [ ] Update CREATING_A_SKILL.md skills dirs
```

- [ ] **Step 4: Commit**

```bash
git add openspec/changes/support-codex-copilot-harnesses/
git commit -m "docs: add openspec change proposal for codex + copilot harnesses"
```

---

### Task 2: Update README harness list

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Read README to find the harness list section**

Run: `grep -n "harness\|opencode\|claude" README.md | head -20`

- [ ] **Step 2: Add codex + copilot to the harness list**

Add codex and copilot alongside opencode and claude-code in any harness list or table. Include their aliases (`cx`, `co`) and exec commands (`codex`, `copilot`).

- [ ] **Step 3: Update the "Adding a new harness" section**

Add a note that codex and copilot are now worked examples, bringing the total to four.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: add codex + copilot to README harness list"
```

---

### Task 3: Update CREATING_A_SKILL.md

**Files:**
- Modify: `CREATING_A_SKILL.md`

- [ ] **Step 1: Find the skills directory listing**

Run: `grep -n "skills\|\.opencode\|\.claude" CREATING_A_SKILL.md | head -20`

- [ ] **Step 2: Add codex + copilot skills dirs**

Add `.codex/skills` / `~/.codex/skills` and `.copilot/skills` / `~/.copilot/skills` to the list of harness skills directories.

- [ ] **Step 3: Commit**

```bash
git add CREATING_A_SKILL.md
git commit -m "docs: add codex + copilot skills dirs to CREATING_A_SKILL.md"
```

---

### Task 4: Verify full test suite passes

- [ ] **Step 1: Run full build and tests**

```bash
go build ./...
go test ./...
```

Expected: all pass, 0 failures.

- [ ] **Step 2: Verify no regressions in existing harnesses**

```bash
go test ./internal/config/ -v -run "TestLookupHarness|TestHarnessAliasesAreUnique|TestApplyServerLaunch|TestResolveInnerCmd"
go test ./internal/session/ -v
```

Expected: all pass.

---

## Self-Review

**1. Spec coverage:** The design spec's 5 sections are covered:
- Section 1 (descriptors): ✓ committed
- Section 2 (bridges): ✓ committed
- Section 3 (session listing): ✓ committed
- Section 4 (manifest generator): ✓ committed (migration deferred)
- Section 5 (docs + testing): Tasks 1-3 cover docs, Task 4 verifies tests

**2. Placeholder scan:** No TBD/TODO in task steps. All steps have concrete actions.

**3. Type consistency:** No type changes in remaining work — all Go code is committed and tested.
