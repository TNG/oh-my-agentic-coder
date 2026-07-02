# Allow/Denylist Provenance Command

- **Issue:** [#34 — Allow/Denylist Provenance Command](https://github.com/TNG/oh-my-agentic-coder/issues/34)
- **Date:** 2026-07-02
- **Status:** Approved, ready for implementation plan

## Problem

omac has four independent allow/denylist subsystems (network, filesystem,
environment, skills), each with two config layers (workdir-local +
user-global) plus a compiled-in baseline. Today there is no way to see the
effective merged rule set in one place. A user who gets a "permission
denied" or an unexpected "allowed" has to manually cross-reference:

- `~/.config/omac/sandbox-profiles/default.json` (and its workdir overlay)
- `default.pages.json` (learned interactive decisions)
- `sidecar.json` (skill registry, two layers)
- the compiled-in protected-path / env-blocklist tables

`omac list` covers skills; `omac config show` covers one skill's config.
Neither shows the sandbox policy surface. This spec adds a single
read-only command that dumps every effective allow/deny entry with its
source.

## Goal

`omac provenance` — one command, four grouped sections (network,
filesystem, environment, skills), each row annotated with the layer it
came from. Table output by default; `--json` for machine parsing.
`--profile <ref>` to target a non-default sandbox profile.

Non-goals (YAGNI):

- Single-item reverse-lookup (`omac explain github.com`). Add when a
  concrete need arises; the dump subsumes it for now.
- Per-subsystem subcommands (`provenance net|fs|...`). Add when scoped
  queries are requested.
- Audit trail with timestamps. Requires logging infrastructure that
  doesn't exist; out of scope.
- Mutation. Provenance is read-only.

## Command surface

```
omac provenance [--profile <ref>] [--json]
```

- `--profile <ref>` — sandbox profile name, path, or `builtin`. Default:
  `default`. Resolved via the existing `sandboxprofile.Resolve(ref)`,
  which returns the profile, the on-disk path it loaded, and any error.
  The sibling learned-decisions file is derived via
  `sandboxprofile.PagesPath(profilePath)`.
- `--json` — emit one JSON object with four arrays instead of tables.
- No positional args. No mutation. Read-only.

Registered in `internal/cli/cli.go` `commands()` map alongside
`list` / `config`. Help text added to `printUsage`.

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | success |
| `2` | misuse (bad flags) |
| `3` | config / profile invalid |
| `5` | I/O error (can't read registry / pages / profile) |

Mirrors the existing `Exit*` constants in `internal/cli/cli.go`.

## Data sources

### Network subsystem

Merged from three sources, emitted in filter-pipeline order so the user
sees the precedence the sandbox actually applies:

| Source | Loader | Entries | SOURCE label |
| --- | --- | --- | --- |
| `default.pages.json` learned decisions | `netprompt.LoadLearnedPolicy(PagesPath(profilePath))` | one row per `LearnedEntry` | `learned` |
| profile `network.allow_domain` | `sandboxprofile.Resolve(ref)` | one row per entry | `workdir` / `global` / `builtin` |
| profile `network.deny_domain` | same profile | one row per entry | same |

Plus static summary rows (non-editable, for transparency):

- Hard-deny metadata hosts (`169.254.169.254`,
  `metadata.google.internal`, `metadata.azure.internal`) — ACTION
  `deny`, SOURCE `builtin`.
- `network.mode` (`filtered` / `blocked` / `open`),
  `network.network_prompt.enabled`, `network.network_prompt.on_unavailable`
  — surfaced as 1-2 summary rows so the user sees the effective policy,
  not just the lists.

### Filesystem subsystem

| Source | Entries | SOURCE label |
| --- | --- | --- |
| profile `filesystem.allow` / `.read` / `.write` | one row each; ACTION maps to `allow` / `read` / `write` | `workdir` / `global` / `builtin` |
| profile `filesystem.deny` | one row, ACTION `deny` | same |
| profile `filesystem.override_deny` | one row, ACTION `override-deny` | same |
| `PlatformBaseline().Read` / `.Write` | one row each | `builtin` |
| `EffectiveProtectedPaths(baseline, overrideDeny)` | one row each, ACTION `deny` (protected) | `builtin` |

`filesystem.allow_unix_dir` folded into the `allow` rows; the ENTRY
column carries a `(unix-dir)` suffix so the grant type is visible without
a separate table.

### Environment subsystem

| Source | Entries | SOURCE label |
| --- | --- | --- |
| `dangerousEnvExact` + `dangerousEnvPrefixes` (from `env.go`) | one row per exact name + one summary row per prefix family; ACTION `deny` | `blocklist` |
| profile `environment.allow_vars` | one row per entry, ACTION `allow` | `workdir` / `global` / `builtin`; empty list = one summary row `(no allowlist — all non-blocklisted vars pass)` |

### Skills subsystem

Reuses `registry.Load(workdir)` + `registry.LoadGlobal()`, merged
workdir-wins (identical to `omac list`):

| Source | Entries | SOURCE label |
| --- | --- | --- |
| each registered skill | one row: NAME, ACTION `registered`, ENTRY carries mount + secret count | `workdir` / `global` |

Stale registrations (skill dir gone) surfaced separately as in
`omac list`, ACTION `stale`.

### Profile layer attribution

`sandboxprofile.Resolve(ref)` returns `(profile, path, err)`. The `path`
identifies which file won. Attribution rule:

- `path` under `<workdir>/.opencode/` → SOURCE `workdir`
- `path` under `~/.config/omac/` → SOURCE `global`
- `path == ""` (compiled-in default, no file scaffolded yet) → SOURCE `builtin`

The launcher config (`oh-my-agentic-coder.yaml`) has its own two-layer
merge for `default_profile` and profile overrides, but the sandbox
profile file is standalone — only the profile path itself is attributed,
not the launcher config's pointer.

## Output formatting

### Text mode (default)

Four sections, each a `tabwriter` table. Section headers in lowercase,
parenthesized with the resolved profile name + effective summary fields.
Empty section prints `(none)`.

```
$ omac provenance

network (profile: default, mode: filtered, prompt: on, on_unavailable: deny)
  ENTRY                        ACTION   SOURCE
  169.254.169.254              deny     builtin
  metadata.google.internal     deny     builtin
  metadata.azure.internal      deny     builtin
  github.com                   allow    workdir
  *.internal.example.com       allow    global
  evil.com                     deny     global
  api.example.com              allow    learned
  badhost.com                  deny     learned

filesystem (profile: default, workdir.access: readwrite)
  ENTRY                        ACTION      SOURCE
  ~/.cache                     allow       builtin
  ~/go                         allow       builtin
  ~/.gitconfig                 read        builtin
  ~/.ssh                       deny        builtin
  ~/.aws                       deny        builtin
  ./config/local               allow       workdir
  .env                         deny        workdir

environment
  ENTRY                        ACTION   SOURCE
  LD_*                         deny     blocklist
  DYLD_*                       deny     blocklist
  BASH_ENV                     deny     blocklist
  PYTHONSTARTUP                deny     blocklist
  ...                          deny     blocklist
  (no allow_vars — all non-blocklisted vars pass)

skills (workdir: /home/user/proj)
  ENTRY                        ACTION      SOURCE
  slack                        registered  workdir
  echo-rest                    registered  global
```

Long ENTRY values (paths) truncated at 60 chars with `…`; the full
value is always available via `--json`. Matches `config show`'s
`displayValue` pattern.

### JSON mode

One object, four arrays. Stable field order via struct tags:

```json
{
  "profile": {
    "name": "default",
    "path": "/home/user/.config/omac/sandbox-profiles/default.json",
    "source": "global"
  },
  "network": {
    "mode": "filtered",
    "prompt_enabled": true,
    "on_unavailable": "deny",
    "entries": [
      {"entry":"169.254.169.254","action":"deny","source":"builtin"},
      {"entry":"github.com","action":"allow","source":"workdir"}
    ]
  },
  "filesystem": {
    "workdir_access": "readwrite",
    "entries": [
      {"entry":"~/.cache","action":"allow","source":"builtin"},
      {"entry":".env","action":"deny","source":"workdir"}
    ]
  },
  "environment": {
    "entries": [
      {"entry":"LD_*","action":"deny","source":"blocklist"}
    ]
  },
  "skills": {
    "workdir": "/home/user/proj",
    "entries": [
      {"entry":"slack","action":"registered","source":"workdir"}
    ]
  }
}
```

## Code structure

### New files

| File | Purpose |
| --- | --- |
| `internal/cli/provenance.go` | `runProvenance`, flag parsing, orchestration, text + JSON formatters. ~200 lines. |
| `internal/cli/provenance_test.go` | Unit tests (table-driven). |
| `internal/e2e/provenance_test.go` | E2E test: provenance output matches actual sandbox behavior. |

One new CLI file. No new package — reuses existing loaders directly:

- `sandboxprofile.Resolve(ref)` → profile + path
- `sandboxprofile.PagesPath(path)` + `netprompt.LoadLearnedPolicy(path)` → learned decisions
- `sandboxprofile.PlatformBaseline()` + `EffectiveProtectedPaths()` → fs baseline + protected paths
- `registry.Load(workdir)` + `registry.LoadGlobal()` → skills (same merge as `list.go`)

### Existing files changed (minimal)

1. `internal/cli/cli.go` — add `"provenance"` entry to `commands()` map + one line in `printUsage`.
2. `internal/sandboxprofile/env.go` — add `DangerousEnvBlocklist() (exact []string, prefixes []string)` accessor. The blocklist tables (`dangerousEnvExact`, `dangerousEnvPrefixes`) are currently unexported; provenance needs to enumerate them for display. ~5 lines, no behavior change.

### No other changes

- No changes to `sandboxrun`, `netproxy`, `netprompt`, `registry`, or
  `config` packages beyond the accessor above.
- No changes to the sandbox profile schema.
- No changes to the launcher config format.

## Testing

### Unit tests (`provenance_test.go`)

Table-driven, mirrors `config_cmd_test.go` / `list_deregister_test.go`:

- Build a temp workdir with a profile + `pages.json` + `sidecar.json`,
  run `runProvenance`, assert text output contains expected rows +
  source labels.
- `--json` mode: parse output, assert struct fields match the
  constructed fixture.
- Profile layer attribution: workdir profile path → SOURCE `workdir`;
  global profile path → SOURCE `global`; no profile file → SOURCE
  `builtin`.
- Empty subsystems print `(none)` (text) / empty arrays (JSON).
- Bad `--profile` ref → `ExitMisuse` / `ExitConfigInvalid`.
- `DangerousEnvBlocklist()` accessor returns non-empty exact + prefix
  slices and the result is stable (no mutation).

### E2E test (`provenance_test.go`, `//go:build e2e`)

New `TestE2EProvenance` — verifies provenance output matches **actual
sandbox behavior**, not just that output is well-formed.

Rationale: provenance claiming `github.com` is allowed while the
sandbox actually denies it is a silent bug. The e2e test cross-checks
each provenance claim against what the running sandbox enforces.

Pattern: reuse the security-audit setup (profile + self-audit skill),
run `omac provenance --json` host-side (no agent needed), then run the
audit agent and cross-check.

Structure:

1. `buildOmac(t)` — compile the binary.
2. `installHarness(t, opencode, home)` — only one harness needed;
   provenance is harness-agnostic.
3. `writeSandboxProfile(t, home, opencode, &spec)` — writes a profile
   with `allow_domain` + `allow_vars` set, so provenance has non-empty
   network + environment sections.
4. `registerSelfAudit(t, omacBin, home, workdir)`.
5. Run `omac provenance --json --profile default` host-side. Parse
   JSON.
6. **Provenance-content assertions** (output matches the profile we
   wrote):
   - Every `allow_domain` entry from the profile appears in
     `provenance.network.entries` with ACTION `allow`.
   - `spec.FsDenyPaths` paths appear as ACTION `deny` in
     `provenance.filesystem.entries` (via the builtin protected-path
     set).
   - `spec.EnvAllowVars` entries appear in
     `provenance.environment.entries` with ACTION `allow`.
   - Blocklist entries (`LD_*`, `BASH_ENV`, etc.) appear with SOURCE
     `blocklist`.
   - `self-audit` skill appears in `provenance.skills.entries` with
     SOURCE `workdir`.
7. **Behavior cross-check** (run the audit agent via `runAuditAgent`):
   - For the denied network domain (`spec.NetDenyDomain`): provenance
     says denied → audit probe shows a network-denial message.
   - For `AUDIT_SECRET`: provenance shows the `allow_vars` list (which
     does not include `AUDIT_SECRET`) plus the blocklist. Since
     `allow_vars` is non-empty and `AUDIT_SECRET` is not in it,
     `AUDIT_SECRET` is implicitly denied → audit probe shows
     `AUDIT_SECRET` absent from agent env.
   - For each `spec.FsDenyPaths`: provenance lists it as denied →
     audit probe shows `Permission denied` (or equivalent).

The cross-check is the key value: if provenance says
`blocked.example.com` is denied but the audit probe shows the agent
reached it, that's a provenance bug (or a sandbox bypass — either way,
caught).

### Skipped

- Multi-harness provenance e2e — provenance reads the same profile
  regardless of harness. One harness (opencode) suffices. Add matrix
  coverage if a harness-specific profile path ever emerges.
- Integration tests beyond the e2e above — the command is pure
  read-only over existing loaders; unit + e2e cover it.
