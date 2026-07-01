# Deferred Items: Manifest Subcommand, Binary Check, Env Overrides

**Date:** 2026-06-29
**Status:** Implemented in PR #24
**Related:** `docs/superpowers/specs/2026-06-29-codex-copilot-backends-design.md`

## Overview

Three items deferred from the codex + copilot harness change are now in scope:

1. `omac manifest` subcommand (hybrid — bridges keep jq, subcommand for debugging)
2. Pre-flight binary check via `exec.LookPath` (blocking in `runLaunch` + `runServe`, advisory in `doctor`)
3. `*_HOME` env overrides for all 4 harnesses (expanded from `COPILOT_HOME`-only per user request)

The openspec proposal at `openspec/changes/support-codex-copilot-harnesses/proposal.md`
lists `COPILOT_HOME` only in its deferred section. This spec expands to all 4 harnesses
for consistency — the proposal/tasks.md will be updated to match.

---

## 1. `omac manifest` subcommand

### Purpose

`manifest.Render()` in `internal/manifest/manifest.go` is tested but never called
from any bridge. Bridges (claude, codex, copilot) render the skills manifest with
inline `jq`. The subcommand makes `manifest.Render()` reachable from the CLI for
debugging, testing, and as a future migration target — without adding a process
spawn to the session-start hot path.

### Interface

```
omac manifest --skills-dir <dir> [--input <file>]
```

- `--skills-dir` (required): the active harness's workdir skills directory
  (e.g. `.claude/skills`, `.codex/skills`, `.copilot/skills`).
- `--input` (optional): path to a file containing the activate-response JSON.
  When omitted, reads from stdin. When the file doesn't exist, prints an
  error to stderr and exits non-zero.

Output: the rendered markdown manifest on stdout. Empty string on JSON parse
failure (matches `Render()` behavior). A warning is printed to stderr on parse
failure for debuggability.

### Implementation

New file `internal/cli/manifest_cmd.go`. Uses `flag.NewFlagSet` +
`reorderFlagsFirst(args)` for flag parsing, matching the established pattern
in `config_cmd.go` and `secrets_cmd.go`:

```go
func runManifest(args []string, env *Env) int {
    fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
    skillsDir := fs.String("skills-dir", "", "active harness skills dir (required)")
    input := fs.String("input", "", "activate-response JSON file (default: stdin)")
    fs.Parse(reorderFlagsFirst(args))
    if *skillsDir == "" {
        fmt.Fprintln(env.Stderr, "manifest: --skills-dir is required")
        return ExitMisuse
    }
    var data []byte
    if *input != "" {
        data, err = os.ReadFile(*input)
        if err != nil { ... error, non-zero exit }
    } else {
        data, err = io.ReadAll(env.Stdin)
        if err != nil { ... error, non-zero exit }
    }
    out := manifest.Render(string(data), *skillsDir)
    fmt.Fprint(env.Stdout, out)
    return ExitOK
}
```

Register in `commands()` map in `cli.go`:

```go
"manifest": {Name: "manifest", Short: "Render the skills manifest from activate-response JSON.", Run: runManifest},
```

Add to `printUsage()` in `cli.go`.

### No bridge migration

Bridges keep inline `jq`. The subcommand exists for parity, debugging, and as
the migration target if we later decide to switch bridges over. Switching would
mean replacing the `render_manifest()` shell function in each bridge with:

```sh
manifest="$(control_post "/__omac__/activate" "{\"dir\":\"${dir}\"}")"
context="$(echo "$manifest" | omac manifest --skills-dir "$skills_dir")"
```

That adds one process spawn per session start. Deferred until profiling shows
the jq duplication is a maintenance burden.

### Testing

- `TestRunManifestStdin`: pipe activate JSON via stdin, verify output matches
  `manifest.Render()`.
- `TestRunManifestInputFile`: same but via `--input /tmp/file.json`.
- `TestRunManifestInputFileNotFound`: `--input /nonexistent` → non-zero exit +
  error message to stderr.
- `TestRunManifestMissingSkillsDir`: `--skills-dir` omitted → non-zero exit.
- `TestRunManifestInvalidJSON`: malformed input → empty stdout, warning on
  stderr, exit 0 (matches `Render()` which returns `""` on parse error).

---

## 2. Pre-flight binary check

### Purpose

Prevent confusing sandbox failures when the inner harness binary isn't
installed. Three integration points: blocking in `runLaunch`, blocking in
`runServe`, advisory in `doctor`.

### 2a. Blocking in `runLaunch`

In `internal/cli/start.go`, `runLaunch()` — after config load (step 1), before
registry reconciliation (step 2):

```go
// 1b. Pre-flight: inner harness binary must be on $PATH.
if innerCmdOverride == "" && len(harness.InnerCmd) > 0 {
    if _, err := exec.LookPath(harness.InnerCmd[0]); err != nil {
        fmt.Fprintf(env.Stderr, "%s: harness binary %q not found on $PATH; install it or pass --inner-cmd <path>\n", prefix, harness.InnerCmd[0])
        return ExitPrerequisiteMissing
    }
}
```

Uses `ExitPrerequisiteMissing` (code 4), not `ExitConfigInvalid` — a missing
binary is a prerequisite problem, matching the exit-code contract used by
`runLaunch` for unregistered skills (line 260).

**Skipped when:**
- `--inner-cmd` override is provided (explicit user override; `innerCmdOverride != ""`)
- `harness.InnerCmd` is empty (defensive — all registered harnesses have one)

**Not skipped when:**
- `--no-sandbox` is set (inner binary must exist regardless of sandbox)

### 2b. Blocking in `runServe`

`runServe` in `internal/cli/serve.go` has its own pipeline — it does NOT call
`runLaunch`. A parallel check is needed after harness + profile resolution
(around line 105 in `serve.go`, after config load):

```go
if len(harness.InnerCmd) > 0 {
    if _, err := exec.LookPath(harness.InnerCmd[0]); err != nil {
        fmt.Fprintf(env.Stderr, "omac serve: harness binary %q not found on $PATH\n", harness.InnerCmd[0])
        return ExitPrerequisiteMissing
    }
}
```

Extract a shared helper `checkInnerBinary(harness, prefix, env) int` called from
both `runLaunch` and `runServe` to avoid duplication. The helper takes the
harness, a diagnostic prefix, and env (for stderr), returns 0 on success or
`ExitPrerequisiteMissing`.

### 2c. Advisory in `omac doctor`

New section in `doctor.go` output, before the existing "Built-in skills" section:

```
Inner harnesses:
  [ok]   opencode    binary=opencode found
  [ok]   claude-code binary=claude found
  [warn] codex       binary=codex not on $PATH
  [warn] copilot     binary=copilot not on $PATH
```

Iterates `config.AllHarnesses()`, runs `exec.LookPath(h.InnerCmd[0])` for each.
Warnings only — does not increment doctor's `failures` counter (existing
behavior: per-skill meta/keychain problems and config/registry errors cause
non-zero exit; this section does not).

Does NOT reuse `installedHarnesses()` from `setup.go` — that function returns
only installed harnesses, but doctor needs to report all registered harnesses.
A single `exec.LookPath` pass over `AllHarnesses()` builds the output.

### Testing

- `TestRunLaunchMissingBinary`: harness with `InnerCmd: ["nonexistent-binary-xyz"]`,
  verify `ExitPrerequisiteMissing` + error message. Uses a binary name guaranteed
  absent from PATH (no mocking needed — `exec.LookPath` on a nonexistent name
  returns an error).
- `TestRunLaunchMissingBinaryWithInnerCmdOverride`: same harness but
  `--inner-cmd /bin/echo`, verify launch proceeds past the check.
- `TestRunServeMissingBinary`: same pattern for `runServe`.
- `TestDoctorHarnessBinarySection`: run doctor, verify output lists all 4
  harnesses with found/not-found status.

---

## 3. `*_HOME` env overrides

### Purpose

Allow overriding each harness's config home directory via an env var.
Needed for test isolation (point harnesses at temp dirs) and for users with
non-standard install locations.

### Semantic: `*_HOME` replaces the full config home

`CLAUDE_HOME=/custom` means `/custom` IS the `.claude` directory — skills go
in `/custom/skills`, not `/custom/.claude/skills`. This replaces the full config
home, not `$HOME`.

For OpenCode (XDG-based): `OPENCODE_HOME=/custom` means `/custom` IS the
opencode config dir — skills go in `/custom/skills`.

### Env vars

| Harness | Env var | Default when unset |
|---------|---------|-------------------|
| opencode | `OPENCODE_HOME` | XDG: `$XDG_CONFIG_HOME/opencode` or `~/.config/opencode` |
| claude-code | `CLAUDE_HOME` | `~/.claude` |
| codex | `CODEX_HOME` | `~/.codex` |
| copilot | `COPILOT_HOME` | `~/.copilot` |

### Implementation

Add `HomeEnv string` field to `Harness` struct in `internal/config/harness.go`:

```go
type Harness struct {
    // ...existing fields...
    // HomeEnv, when non-empty, names an environment variable whose value
    // replaces the harness's full config home directory. When the env var
    // is unset or empty, the harness falls back to its default config home
    // (UserConfigHome under $HOME, or XDG for opencode).
    HomeEnv string
}
```

Set in registry:

```go
{ Name: "opencode",     ..., HomeEnv: "OPENCODE_HOME" },
{ Name: "claude-code",  ..., HomeEnv: "CLAUDE_HOME" },
{ Name: "codex",        ..., HomeEnv: "CODEX_HOME" },
{ Name: "copilot",       ..., HomeEnv: "COPILOT_HOME" },
```

Add an exported `ConfigHome()` method on `Harness` — exported because session
path resolvers in `package session` need to call it:

```go
// ConfigHome returns the harness's full config home directory, honoring the
// HomeEnv override. For UserConfigHome harnesses (claude, codex, copilot),
// this is $HOME/<UserConfigHome> by default. For XDG harnesses (opencode),
// this is $XDG_CONFIG_HOME/<base> or ~/.config/<base> by default.
// When HomeEnv is set and non-empty, its value replaces the default entirely.
// Returns "" when no home can be resolved.
func (h Harness) ConfigHome() string {
    if h.HomeEnv != "" {
        if dir := os.Getenv(h.HomeEnv); dir != "" {
            return dir
        }
    }
    base := h.SkillsBase
    if base == "" {
        base = SharedSkillsBase
    }
    if h.UserConfigHome != "" {
        home, err := os.UserHomeDir()
        if err != nil || home == "" {
            return ""
        }
        return filepath.Join(home, h.UserConfigHome)
    }
    // XDG (opencode)
    root := userConfigRoot()
    if root == "" {
        return ""
    }
    return filepath.Join(root, base)
}
```

### Sites that use it

**`GlobalSkillsDir()` in `harness.go`:**

Current (both branches use `os.UserHomeDir()` or `userConfigRoot()` directly).

New — both branches go through `ConfigHome()`:

```go
func (h Harness) GlobalSkillsDir() string {
    home := h.ConfigHome()
    if home == "" {
        return ""
    }
    return filepath.Join(home, "skills")
}
```

This simplifies the method: `ConfigHome()` handles both UserConfigHome and XDG
cases, so `GlobalSkillsDir()` just appends `"skills"`.

**`GlobalBridgeDir()` in `harness.go`:**

Currently uses `userConfigRoot()` directly for the opencode case. Update to use
`ConfigHome()` so `OPENCODE_HOME` is honored:

```go
func (h Harness) GlobalBridgeDir() string {
    if h.BridgeDir == "" {
        return ""
    }
    leaf := filepath.Base(h.BridgeDir)
    base := h.SkillsBase
    if base == "" {
        base = SharedSkillsBase
    }
    if leaf == "."+base || leaf == base {
        return ""
    }
    home := h.ConfigHome()
    if home == "" {
        return ""
    }
    return filepath.Join(home, leaf)
}
```

**Session listing path resolvers in `session.go`:**

`claudeProjectsRoot`, `codexSessionsRoot`, and `copilotDBPath` each gain a
`h config.Harness` parameter and use `h.ConfigHome()`. `opencodeDBPath()`
stays unchanged (no env override for the data dir — see Out of scope).

The `List()` function already receives the harness; its body changes to pass
`h` to the resolvers:

```go
func List(h config.Harness, workdir string) ([]Session, error) {
    return list(h, workdir, execRunner,
        claudeProjectsRoot(h), opencodeDBPath(),
        codexSessionsRoot(h), copilotDBPath(h))
}
```

```go
func claudeProjectsRoot(h config.Harness) string {
    home := h.ConfigHome()
    if home == "" {
        return ""
    }
    return filepath.Join(home, "projects")
}

func codexSessionsRoot(h config.Harness) string {
    home := h.ConfigHome()
    if home == "" {
        return ""
    }
    return filepath.Join(home, "sessions")
}

func copilotDBPath(h config.Harness) string {
    home := h.ConfigHome()
    if home == "" {
        return ""
    }
    return filepath.Join(home, "session-store.db")
}
```

### Known limitations (out of scope, tracked for follow-up)

- **`skillsource.userGlobalRoots()`** independently computes `os.UserHomeDir()`
  + XDG paths for skill discovery. `HomeEnv` overrides won't be reflected in
  skillsource's global root scanning until that function is wired to
  `ConfigHome()`. Follow-up item.
- **Sandbox profile `DefaultProfile()`** hardcodes `~/.claude`, `~/.codex`,
  `~/.copilot`, `~/.config/opencode` in filesystem allowlists. When `HomeEnv`
  overrides redirect the actual paths, the sandbox mounts the `~/.` defaults.
  The sandbox `expand.go` would need `HomeEnv`-aware `~` expansion. Follow-up.
- **`opencodeDBPath()`** stays at `~/.local/share/opencode/opencode.db` — the
  env override applies to the config/skills dir, not the data dir. If needed
  later, a separate `OPENCODE_DATA_HOME` can be added.

### Testing

All env-var tests use `t.Setenv()` (Go 1.17+) for safe per-test env mutation:

- `TestConfigHomeEnvOverride`: set `CODEX_HOME=/tmp/x`, verify
  `ConfigHome()` returns `/tmp/x`.
- `TestConfigHomeEnvOverrideUnset`: unset env, verify default path
  (`$HOME/.codex`).
- `TestGlobalSkillsDirEnvOverride`: set `CLAUDE_HOME=/tmp/c`, verify
  `GlobalSkillsDir()` returns `/tmp/c/skills` (NOT `/tmp/c/.claude/skills`).
- `TestGlobalSkillsDirEnvOverrideOpenCode`: set `OPENCODE_HOME=/tmp/oc`,
  verify `GlobalSkillsDir()` returns `/tmp/oc/skills`.
- `TestSessionListEnvOverride`: set `COPILOT_HOME=/tmp/y`, create a fake
  `session-store.db` there, verify `listCopilot` reads from it.
- `TestSessionListEnvOverrideUnset`: unset env, verify default path.

---

## Scope

### In scope
- `omac manifest` subcommand (read JSON, render markdown)
- Pre-flight binary check in `runLaunch` + `runServe` (blocking,
  `ExitPrerequisiteMissing`)
- Harness binary status section in `omac doctor` (advisory)
- `HomeEnv` field on `Harness` descriptor, all 4 harnesses
- `ConfigHome()` exported method, wired into `GlobalSkillsDir()`,
  `GlobalBridgeDir()`, session path resolvers

### Out of scope
- Migrating bridge scripts from jq to `omac manifest` (deferred again — profiling needed)
- `OPENCODE_DATA_HOME` for the opencode session DB (YAGNI — config dir override is enough for now)
- `skillsource.userGlobalRoots()` wiring (follow-up)
- Sandbox profile `HomeEnv`-aware path expansion (follow-up)
- Windows path handling nuances (existing code uses `os.UserHomeDir()` uniformly; env override follows the same pattern)

### Ordering dependencies

Items 2 and 3 both modify `internal/config/harness.go` (item 2 reads
`InnerCmd`; item 3 adds `HomeEnv` field). No functional conflict, but sequence
them or co-commit to avoid merge conflicts. Item 2's shared helper
`checkInnerBinary` goes in `start.go`/`serve.go`, not `harness.go` — no
conflict there.

### Testing strategy

TDD throughout. Each item has a failing test first, then implementation to
green. All tests run in `go test ./...` — no external dependencies.
