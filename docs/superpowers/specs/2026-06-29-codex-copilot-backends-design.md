# Design: Codex + Copilot CLI backends

**Date:** 2026-06-29
**Status:** Approved (brainstorming complete)
**Branch:** `feat/codex-copilot-backends`
**Worktree:** `.worktrees/codex-copilot-backends`

## Goal

Allow Codex CLI and GitHub Copilot CLI as backends (harnesses) in omac.
All omac features (skills manifest injection, per-directory activate/deactivate,
session continue/resume, facade access) work with both.

## Context

The codebase already has a clean, declarative harness abstraction in
`internal/config/harness.go`. Adding a harness means:

1. Appending one `Harness` descriptor to `harnessRegistry()`.
2. Shipping a client-side bridge (hook script + registration) that calls the
   omac control plane at session lifecycle boundaries.
3. Adding a `SessionListKind` enum value + the matching `case` in
   `internal/session/session.go` if the harness has a session store.

Claude Code was added this exact way
(`openspec/changes/support-claude-code-harness`). This change is the literal
continuation: two more harnesses, same pattern, no new abstraction.

## Verified harness facts

### Codex CLI (OpenAI)

- Exec: `codex`
- Config home: `~/.codex/` (`config.toml`)
- Skills dir: `~/.codex/skills` (global), `.codex/skills` (repo)
- Hooks: `~/.codex/hooks.json` or inline `[hooks]` in `config.toml`; repo-level
  `<repo>/.codex/hooks.json`
- Hook events: `SessionStart`, `Stop`, `PreToolUse`, `PostToolUse`, etc.
  (near-exact mirror of Claude Code's hook system)
- SessionStart payload: `{session_id, cwd, source, ...}` on stdin
- SessionStart output: `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"<text>"}}`
  — same nesting as Claude Code
- Resume: `codex resume` (most recent), `codex resume <id>` (by id)
- Sessions stored on disk under `~/.codex/sessions/`
- Hooks require trust review before running (`/hooks` in TUI, or
  `--dangerously-bypass-hook-trust`)

### GitHub Copilot CLI

- Exec: `copilot` (installed via `npm install -g @github/copilot`,
  `curl -fsSL https://gh.io/copilot-install | bash`, Homebrew, or WinGet)
- Config home: `~/.copilot/` (override: `COPILOT_HOME` env var)
- Skills dir: `~/.copilot/skills/` (global, each in subdir with `SKILL.md`)
- Hooks: `~/.copilot/hooks/*.json` (user), `.github/hooks/*.json` (repo)
- Hook events: `sessionStart`, `sessionEnd`, `agentStop`, `preToolUse`,
  `postToolUse`, `userPromptSubmitted`, etc. (camelCase; PascalCase aliases
  accepted)
- sessionStart payload (camelCase): `{sessionId, timestamp, cwd, source, ...}`
- sessionStart output: `{"additionalContext":"<text>"}` — **flatter** than
  Claude/Codex. `additionalContext` is a top-level key, NOT nested under
  `hookSpecificOutput`.
- Hooks reload on CLI start (not hot)
- No documented trust-review gate for repo-level `.github/hooks/` hooks
- Resume:
  - `copilot --continue` — most recent session (cwd-scoped), no arg
  - `copilot --resume` — picker (no arg), or `--resume <id|prefix|name>` (fuzzy)
  - `copilot --session-id <ID>` — exact ID, no fuzzy matching
- Session store: `~/.copilot/session-state/{id}/events.jsonl` (per-session)
  + `~/.copilot/session-store.db` (SQLite cross-session index)

## Approach

**Approach A (full):** Two new descriptors + two native bridges + shared
manifest generator extraction + two new SessionListKind values + docs.

Rejected alternatives:
- **B (MCP server bridge):** Both CLIs support MCP, but omac's control plane is
  REST over Unix socket. Introducing an MCP server is a new subsystem, more code
  than two hook scripts. Defer as a later unification.
- **C (descriptors only, no bridges):** Smallest diff, but without bridges the
  per-directory activate/deactivate and manifest-in-context do nothing. Does
  not meet "all features should work" requirement.

## Design

### Section 1: Harness registry descriptors

Two entries appended to `harnessRegistry()` in `internal/config/harness.go`:

```go
// codex — OpenAI Codex CLI
{
    Name:           "codex",
    Aliases:        []string{"cx"},
    InnerCmd:       []string{"codex"},
    ServerLaunch:   nil,  // codex app-server is experimental; defer
    BridgeDir:       ".codex",
    SkillsBase:     "codex",
    UserConfigHome: ".codex",  // ~/.codex, not ~/.config/codex
    Session: &HarnessSession{
        ContinueArgs:   []string{"resume"},
        ResumeByIDArgs: func(id string) []string { return []string{"resume", id} },
        ListKind:       SessionListCodex,
    },
},
// copilot — GitHub Copilot CLI
{
    Name:           "copilot",
    Aliases:        []string{"co"},
    InnerCmd:       []string{"copilot"},
    ServerLaunch:   nil,
    BridgeDir:       ".copilot",
    SkillsBase:     "copilot",
    UserConfigHome: ".copilot",  // ~/.copilot, not ~/.config/copilot
    Session: &HarnessSession{
        ContinueArgs:   []string{"--continue"},
        ResumeByIDArgs: func(id string) []string { return []string{"--session-id", id} },
        ListKind:       SessionListCopilot,
    },
},
```

Both use `UserConfigHome` (like Claude Code) since both put config at
`~/.codex` / `~/.copilot` rather than `~/.config/<base>`.

Two new `SessionListKind` enum values: `SessionListCodex`, `SessionListCopilot`.

No other Go core changes. `splitHarnessToken`, `ResolveInnerCmd`,
`ApplyServerLaunch`, `filterRegistryByHarness`, `findUnregisteredSkills`,
`ensureBuiltinSkills`, `WorkdirSkillsDir`, `GlobalSkillsDir` — all consume the
registry and work unchanged.

### Section 2: Bridge mechanism per harness

Each bridge does these things at session lifecycle boundaries:

1. **SessionStart** → `POST /__omac__/activate` with `{cwd, harness, session_id}`,
   receive manifest+env JSON, inject as `additionalContext` into the agent.
2. Mid-session: manifest is in context, skills call the facade via `OMAC_SOCKET`.
3. **SessionEnd** (copilot only) → `POST /__omac__/deactivate`.

**Codex has no SessionEnd event.** Codex's `Stop` event is turn-scoped (fires
every turn end), NOT session-scoped. Using it for deactivate would kill
mid-session skill access. Codex relies on omac's TTL-based reaper for
cleanup (same as a crash). This is a known limitation, documented in the bridge
script.

#### Codex bridge

One file: `.codex/hooks/omac-bridge.sh` + a `.codex/hooks.json` registering it
for `SessionStart` only (not `Stop`). Based on `.claude/hooks/omac-bridge.sh`
with the output shape unchanged (codex uses the same
`hookSpecificOutput.additionalContext` nesting).

No `Stop`/deactivate handler — codex `Stop` is turn-scoped, not session-scoped.

Codex hooks require trust review before running. `omac start codex` prints a
one-time notice: "Run `/hooks` in codex to trust the omac bridge, or pass
`--dangerously-bypass-hook-trust`." Same UX trade-off as Claude Code today.

#### Copilot bridge

One file: `.copilot/hooks/omac-bridge.sh` + a `.copilot/hooks/omac.json`
(user-level, not repo-level) registering it for `SessionStart` and
`SessionEnd`.

Registered with PascalCase event names (`SessionStart`, `SessionEnd`) so the
payload uses snake_case fields (`hook_event_name`, `session_id`, `cwd`) —
compatible with the claude bridge's dispatch logic. The output shape uses
`hookSpecificOutput.additionalContext` nesting (VS Code-compatible format,
same as claude/codex).

**User-level hooks, not repo-level.** The registration json lives at
`.copilot/hooks/omac.json` (user-level `~/.copilot/hooks/`), NOT at
`.github/hooks/omac.json` (repo-level). This avoids committing absolute paths
to git and the portability issues that causes. Uses git-root-relative path
for the script reference.

Copilot hooks reload on CLI start (not hot) — same as codex. No trust-review
gate documented for copilot hooks. Simpler than codex/claude in this respect.

#### Bridge directory layout

```
.codex/hooks/omac-bridge.sh          # SessionStart handler only (no Stop)
.codex/hooks.json                    # registers omac-bridge.sh for SessionStart
.copilot/hooks/omac-bridge.sh         # SessionStart + SessionEnd handlers
.copilot/hooks/omac.json             # user-level registration (not .github/hooks/)
```

Both bridges use git-root-relative paths (`$(git rev-parse --show-toplevel)`)
for script references, ensuring portability across machines.

#### What's NOT in the bridge

- No MCP server. Both CLIs support MCP, but omac's control plane stays REST
  over Unix socket. The bridges are shell scripts, not MCP servers.
- No per-harness config file parsing. The bridge is a self-contained shell
  script + a registration file. No Go code reads codex's `config.toml` or
  copilot's `settings.json`.
- No session-state parsing in the bridge. Session listing is a Go-side concern
  (Section 3), not the bridge's job.

### Section 3: Session listing

Two new `SessionListKind` values + their `case` branches in
`internal/session/session.go`. The `session.List` switch already dispatches by
`ListKind`; each branch reads the harness's session store and returns
`[]SessionInfo`.

#### SessionListCodex

Codex stores sessions on disk under `~/.codex/sessions/`. Implementation reads
the directory, parses each session's metadata (id, created, first-message
summary), returns the list. Matches the existing pattern where
`SessionListOpenCodeCLI` reads opencode's session store.

```go
case SessionListCodex:
    return listCodexSessions(ctx, homeDir)
```

`listCodexSessions` walks `~/.codex/sessions/`, reads each session file's header
for id + timestamp + summary, returns sorted by last-modified. No external
process spawn — direct file read, same as the opencode path.

#### SessionListCopilot

Copilot stores sessions two ways:
- `~/.copilot/session-state/{id}/events.jsonl` — full event log per session
- `~/.copilot/session-store.db` — SQLite cross-session index

The SQLite db is the authoritative index (opencode's `SessionListOpenCodeCLI`
also reads a SQLite db). `listCopilotSessions` queries `session-store.db` for
`(id, name, created_at, last_used_at)`, returns sorted by last-used.

```go
case SessionListCopilot:
    return listCopilotSessions(ctx, copilotHome)
```

Both shell out to the `sqlite3` CLI binary (same pattern as opencode's existing
`listOpenCodeDB`). No new Go dependency — the existing codebase already shells
out to `sqlite3` for opencode session listing. The copilot path schema-gates
with `PRAGMA table_info` and returns nil gracefully when columns are absent,
since the copilot SQLite schema is undocumented.

#### What's NOT here

- No parsing of `events.jsonl` content for listing — the index/db has the
  metadata. The event log is only relevant for resume, which the CLI itself
  handles.
- No session-state writing. omac never writes to codex's or copilot's session
  stores. Listing is read-only.
- No fallback to shelling out to `codex`/`copilot` for a session list. Direct
  file/db read, consistent with how opencode listing works today.

### Section 4: Shared manifest/env generator extraction

Today the manifest+env text is rendered in two places:
- `.opencode/plugins/omac-multidir.ts` (TypeScript, in opencode plugin)
- `.claude/hooks/omac-bridge.sh` (bash, in claude hook)

Both render the same content: the active skills manifest (names + descriptions
+ env vars), the facade socket path, and the OMAC_* env vars. They've drifted
and will drift further with two more bridges.

#### Extraction

One Go function in `internal/manifest/` (new package, ~1 file):

```go
package manifest

// Render returns the manifest text injected into an agent's context
// at SessionStart. Harness-agnostic. Bridges wrap it in their
// harness-specific JSON envelope.
func Render(socketPath string, skills []SkillInfo, env []EnvVar) string
```

`SkillInfo` and `EnvVar` are small structs (name, description, value). The
function returns a plain string — the manifest body. It does NOT know about
`hookSpecificOutput` or `additionalContext` envelopes. Each bridge wraps the
string in its own envelope shape.

The four bridges (opencode plugin, claude hook, codex hook, copilot hook) all
call this same function via a small CLI:

```go
// Usage: omac manifest --socket $OMAC_SOCKET
// Prints the rendered manifest string to stdout.
```

Each bridge script becomes:
```bash
MANIFEST=$(omac manifest --socket "$OMAC_SOCKET")
# wrap in harness-appropriate JSON envelope, emit on stdout
```

#### Why a Go binary, not re-implement in bash/ts

The existing bridges duplicate the rendering logic in bash and TypeScript. The
manifest logic already lives in omac's Go core (the control plane has it).
Exposing it via a subcommand is one function + one CLI flag. Shorter than
re-implementing in bash/ts. Fewer files than keeping two copies. Correct call.

#### What the manifest contains (unchanged from current)

```
# omac active skills

## <skill-name>
<description>
Environment: OMAC_<SKILL>_TOKEN, OMAC_<SKILL>_URL, ...

# facade
Unix socket: /path/to/omac.sock
```

No behavior change from what the existing bridges render. Just single-sourced.

#### Migration of existing bridges

DEFERRED: The opencode plugin and claude hook migration to `omac manifest` is
split into a separate change to avoid scope creep risk to working harnesses
(finding #16 from review). The `internal/manifest/Render()` function exists
and is tested, but the existing bridges keep their inline render logic for
now. The new codex and copilot bridges also use inline render logic (matching
the existing pattern) until the migration change lands.

### Section 5: Docs + testing

#### Docs

- **README.md**: add codex + copilot to the harness list. Update the "Adding a
  new harness" note — now four worked examples (opencode, claude, codex,
  copilot) instead of two.
- **CREATING_A_SKILL.md**: list all four harnesses' skills dirs
  (`.opencode/skills`, `.claude/skills`, `.codex/skills`, `.copilot/skills` +
  global `~/.codex/skills`, `~/.copilot/skills`).
- **docs/superpowers/specs/**: this design doc (committed).
- **openspec/changes/support-codex-copilot-harnesses/**: new openspec change
  proposal mirroring the claude one.

#### Testing

Non-trivial logic leaves ONE runnable check. The existing codebase has
`go test ./...` — so the checks fit there, no new framework.

1. **`internal/config/harness_test.go`** (extend or add): assert the registry
   now has 4 harnesses, codex + copilot present with correct `InnerCmd`,
   `BridgeDir`, `Session.ListKind`, `UserConfigHome`. This is the load-bearing
   fact — if the descriptors are wrong, everything breaks.

2. **`internal/session/session_test.go`** (extend): add cases for
   `SessionListCodex` and `SessionListCopilot`. Use a fixture dir (a fake
   `~/.codex/sessions/` and a fake `session-store.db`), assert listing returns
   expected sessions. Tests the two new switch branches.

3. **`internal/manifest/manifest_test.go`** (new): assert `Render()` produces
   stable output with given skills + env. One test, one assertion on output
   shape. Tests the extracted generator before the bridges depend on it.

No bridge-script tests — they're shell scripts calling `omac manifest`, and
testing bash wrappers is more code than the scripts. The Go logic they call is
tested. Matches how the existing claude/opencode bridges aren't unit-tested.

#### What's NOT tested

- Bridge scripts themselves (shell, not Go — same as existing).
- End-to-end "launch codex, verify manifest appears in context" — requires the
  codex binary + a live session. Manual smoke test, not automated. Same bar as
  the claude change had.

## Out of scope (explicitly deferred)

- Codex `app-server` / copilot cloud sessions (`ServerLaunch: nil` for both).
- MCP server bridge (Approach B, rejected).
- Per-skill harness compatibility matrix — skills are harness-agnostic by the
  existing design's guarantee.

## File impact summary

| File | Change |
|------|--------|
| `internal/config/harness.go` | +2 descriptors, +2 `SessionListKind` enum values |
| `internal/session/session.go` | +2 `case` branches (`listCodex`, `listCopilot`), +2 params to `list()` |
| `internal/manifest/` (new pkg) | `Render()` func (extraction, tested) |
| `.codex/hooks/omac-bridge.sh` (new) | SessionStart-only hook script (no Stop) |
| `.codex/hooks.json` (new) | registers omac-bridge.sh for SessionStart |
| `.copilot/hooks/omac-bridge.sh` (new) | SessionStart + SessionEnd hook script |
| `.copilot/hooks/omac.json` (new) | user-level registration (not .github/hooks/) |
| `internal/config/harness_test.go` | extend: 4 harnesses, codex + copilot fields |
| `internal/session/session_test.go` | extend: codex + copilot dispatch + missing-store tests |
| `internal/manifest/manifest_test.go` (new) | `Render()` output stability |
| `openspec/changes/support-codex-copilot-harnesses/` (new) | proposal + tasks |

Deferred to separate change:
- `omac manifest` subcommand + migration of opencode/claude bridges
- README.md + CREATING_A_SKILL.md docs updates
