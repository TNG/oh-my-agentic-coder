# Design: `omac provenance --check` — Static Security Lint

**Date:** 2026-07-06
**Origin:** Reviewer comment on PR #36 (`feat/provenance-command`) by NoRiceToday:

> does it make sense to add an optional flag which results in a static check of the whitelist vs known secret locations/'attack vectors'?

**Status:** Approved (brainstormed 2026-07-06).

## Context

PR #36 ships `omac provenance` — a read-only dump of every effective allow/deny entry across network, filesystem, environment, and skills subsystems. The reviewer asked for an optional flag that lints the resolved profile against known secret locations and attack vectors, surfacing risky grants before launch.

PR #38 (`37-add-security-audit-trail`) ships a runtime event trail under `internal/audit/` and `internal/cli/audit.go` with `--audit-log` / `--no-audit` / `--audit-strict` flags on `start`/`serve`. That feature answers "what did the agent do?"; this feature answers "is the allow/deny config safe?". To avoid collision on package name, CLI file, and the `--audit` flag word, this feature lives in a separate package `internal/profileaudit/` and uses the flag `--check`.

## Goal

Add `omac provenance --check` — a non-blocking, static security lint of the resolved sandbox profile. Runs entirely offline; no filesystem scan, no network calls, no agent execution. CI-friendly: exit code 2 if any HIGH finding, 0 otherwise.

## Non-goals

- Runtime audit trail (covered by PR #38).
- Environment-variable passthrough linting (deferred; not requested).
- Workdir filesystem scan for actual secret files (deferred; requires touching the workdir at audit time).
- Blocking launch (`--check` is advisory; `omac start` is unaffected).

## Interface

```
omac provenance [--profile <ref>] [--check] [--json]
```

| Flag | Description |
|------|-------------|
| `--check` | Switch from dump mode to lint mode. Runs the static check and prints findings instead of the allow/deny tables. |
| `--profile <ref>` | Same as today: profile name, path, or `""` for default. Audits the saved profile file + baseline, no CLI flag overrides (matches provenance's existing contract). |
| `--json` | Reuses the existing flag. In check mode emits a JSON array of findings instead of text. |

**Exit code:** `0` if no HIGH findings, `ExitConfigInvalid` (2) if any HIGH present. MEDIUM/LOW never fail the exit code.

**No new flags** beyond `--check`. The audit reflects the saved profile as resolved by `sandboxprofile.Resolve` + `PlatformBaseline` + `EffectiveProtectedPaths`. Flag overrides from `omac sandbox run` (`--allow`, `--deny`, `--open-port`, …) are **not** accepted; the audit sees the profile file as written. This keeps provenance's surface minimal and matches what CI would check.

## Package layout

New package `internal/profileaudit/` (avoids `internal/audit` collision with PR #38):

```
internal/profileaudit/
  knownsecrets.go        # curated secret-path + glob tables (clearly separated)
  knownsecrets_test.go
  check.go               # Check() → []Finding
  check_test.go
  finding.go             # Finding type, severity constants, ExitCode()
  finding_test.go
```

`internal/cli/provenance.go` — add `--check` flag + dispatch to `profileaudit.Check`, then `writeCheckText/JSON`. ~30 lines added; no other CLI files touched.

### Dependency direction

```
internal/cli (provenance.go)
   └── internal/profileaudit
          └── internal/sandboxprofile   (Profile, Baseline, EffectiveProtectedPaths, ExpandPath)
```

`profileaudit` depends only on `sandboxprofile` (domain types). No import cycle: `cli` imports `profileaudit`, `profileaudit` imports `sandboxprofile`, `sandboxprofile` imports nothing from either.

## Data model

### `knownsecrets.go` — two clearly separated tables

The two-list model (decision from Q3/C) surfaces two distinct risks:

1. A profile weakens a **baseline** protection (category E — `override_deny`).
2. A profile exposes a path omac **never protected** in the first place (category A — extension list).

```go
// BaselineSecretPaths wraps sandboxprofile.PlatformBaseline().ProtectedPaths.
// These are paths omac already denies by default. Exported so the check
// can distinguish "profile weakens a baseline protection" (category E)
// from "profile exposes a path omac never protected" (category A).
func BaselineSecretPaths() []string

// ExtensionSecretPaths are known secret-bearing paths NOT in the baseline.
// Curated, small list. Add new entries here only.
var ExtensionSecretPaths = []string{
    "~/.pypirc",
    "~/.config/github-copilot",
    "~/.config/gh",
    "~/.gitconfig",
    "~/.config/hub",
    "~/.cf",
}

// SecretBasenameGlobs are filename patterns matched against grants
// that use wildcards or broad directory grants (e.g. allow: ["."]).
var SecretBasenameGlobs = []string{
    ".env", "*.env", "*.key", "*.pem",
    "*token*", "*secret*", "id_rsa*",
    "*.pfx", "*.p12",
}
```

**Maintenance:** the two tables are kept in separate vars with clear doc comments. A guard test (`knownsecrets_test.go`) asserts that `ExtensionSecretPaths` has no entry that already appears in `BaselineSecretPaths()`, catching drift if the baseline grows.

### `finding.go`

```go
type Severity string

const (
    SeverityHigh   Severity = "high"
    SeverityMedium Severity = "medium"
    SeverityLow    Severity = "low"
)

type Category string

const (
    CatFSGrant      Category = "filesystem"     // category A
    CatNetwork      Category = "network"         // category C
    CatOverrideDeny Category = "override_deny"   // category E
)

type Finding struct {
    Severity Severity `json:"severity"`
    Category Category `json:"category"`
    Field    string   `json:"field"`    // e.g. "filesystem.allow"
    Value    string   `json:"value"`     // e.g. "~/.ssh"
    Message  string   `json:"message"`   // one-line explanation
}

// ExitCode returns ExitConfigInvalid (2) if any finding is HIGH, else 0.
func ExitCode(findings []Finding) int
```

## Check logic

Single entry point:

```go
func Check(profile *sandboxprofile.Profile) []Finding
```

Internally calls `sandboxprofile.PlatformBaseline()` and `EffectiveProtectedPaths(base, profile.Filesystem.OverrideDeny)`. Three check functions, results merged and sorted (high → medium → low, then by category, then by field).

### `checkFSGrants` — category A

For each entry in `profile.Filesystem.Allow`, `.Read`, `.Write`, `.AllowUnixDir`:

1. **Explicit-path grants:** expand the grant path via `sandboxprofile.ExpandPath`. Compare against `BaselineSecretPaths() ∪ ExtensionSecretPaths`. If the grant path **equals or is a parent of** a known secret path → finding (a parent grant exposes the secret path beneath it). A grant that is a *subpath* of a secret path (e.g. granting `~/.ssh/foo`) does **not** expose `~/.ssh` itself and is not flagged.
   - Grant overlaps a **baseline** secret path → **high** (profile actively exposes something omac protects).
   - Grant overlaps an **extension** secret path → **medium** (omac doesn't protect this, but it's risky).
2. **Broad/glob grants:** if the grant entry is `"*"`, `"."`, `"./"`, or otherwise not an explicit path (no `~`/`$VAR` expansion, contains a glob metacharacter), the grant could expose any file beneath it. In that case, emit one **medium** finding per `SecretBasenameGlob` that could plausibly match a filename under the grant (e.g. `allow: ["."]` + glob `.env` → `[MEDIUM] filesystem.allow: "." — broad grant may expose ".env" files`). This is potential, not certain; no filesystem scan confirms the file exists. Cap the findings per broad grant at one per glob to avoid noise.

Field naming: `filesystem.allow`, `filesystem.read`, `filesystem.write`, `filesystem.allow_unix_dir` — matches the provenance table's `ACTION` column vocabulary.

### `checkNetwork` — category C

For each entry in `profile.Network.AllowDomain`:

- Exact match against cloud-metadata blocklist (`169.254.169.254`, `metadata.google.internal`, `metadata.azure.internal`) → **high**.
- Match against `*.internal` / `*.local` heuristic (SSRF surface) → **medium**.

For ports:
- `OpenPort` containing `0` (any loopback port) → **low**.
- `AllowTCPConnect` to `22` (SSH) or `3389` (RDP) → **medium**.

Field naming: `network.allow_domain`, `network.open_port`, `network.allow_tcp_connect`.

### `checkOverrideDeny` — category E

For each entry in `profile.Filesystem.OverrideDeny`:

- If it removes a path in `BaselineSecretPaths()` → **high**. The finding's `Message` names the specific protection removed, e.g. `"override_deny removes baseline protection on ~/.ssh (SSH private keys)"`.

Field naming: `filesystem.override_deny`.

## CLI integration

In `provenance.go`'s `runProvenance`:

```go
checkMode := fs.Bool("check", false, "Static security lint of the resolved profile.")
// ...after resolving profile via sandboxprofile.Resolve...
if *checkMode {
    findings := profileaudit.Check(profile)
    if *jsonOut {
        return writeCheckJSON(env.Stdout, findings)
    }
    return writeCheckText(env.Stdout, findings)
}
```

`writeCheckText`: one finding per line, sorted by severity (high → low), then category, then field. Format: `[HIGH] <field>: "<value>" — <message>`. Human-readable, greppable.

`writeCheckJSON`: `[]Finding` array via `json.MarshalIndent`, trailing newline (house style).

## Output examples

### Text

```
[HIGH]   filesystem.allow: "~/.ssh" — intersects baseline protected path (SSH private keys)
[HIGH]   filesystem.override_deny: "~/.aws" — removes baseline protection (AWS credentials)
[MEDIUM] filesystem.allow: "~/.pypirc" — overlaps known secret path not in baseline (PyPI upload token)
[MEDIUM] network.allow_domain: "169.254.169.254" — cloud metadata endpoint (credential theft surface)
[LOW]    network.open_port: 0 — any loopback port
```

### JSON

```json
[
  {
    "severity": "high",
    "category": "filesystem",
    "field": "filesystem.allow",
    "value": "~/.ssh",
    "message": "intersects baseline protected path (SSH private keys)"
  }
]
```

Empty result (clean profile):

- Text: `(no findings)` on stdout, exit 0.
- JSON: `[]` on stdout, exit 0.

## Testing

### Unit tests in `internal/profileaudit/`

- **`knownsecrets_test.go`** — verify `ExtensionSecretPaths` has no overlap with `BaselineSecretPaths()` (guard against drift). Verify `SecretBasenameGlobs` entries are valid `filepath.Match` patterns.
- **`check_test.go`** — table-driven. Feed synthetic `*sandboxprofile.Profile` values, assert findings + severities + exit code. Covers:
  - All three categories (A, C, E).
  - All three severities (high, medium, low).
  - Edge cases: empty profile (no findings), broad-glob grant (`allow: ["."]`), metadata domain, `override_deny` on a baseline path, extension path grant, clean default profile.
- **`finding_test.go`** — `ExitCode` logic: empty → 0, only-low → 0, only-medium → 0, any-high → 2.

### CLI tests in `internal/cli/provenance_test.go` (extend existing)

- `omac provenance --check` on the default profile → exit 0, output contains `(no findings)`.
- `omac provenance --check --json` → valid JSON, empty array on clean profile.
- A crafted risky profile (via `--profile <path>` pointing at a temp file with `allow: ["~/.ssh"]`) → exit 2, text output contains `[HIGH]`.

### Platform note

`PlatformBaseline()` is OS-specific (darwin vs linux). Tests that assert specific paths use the `BaselineSecretPaths()` wrapper (which delegates to the platform function) and avoid hardcoding darwin-only paths like `~/Library/Keychains`. Tests that need a deterministic baseline construct a synthetic `*Profile` and call `Check` directly rather than relying on the real platform baseline.

## Open questions / future work

- **Env-var passthrough linting** — deferred; not requested in this round. The `DangerousEnvBlocklist` already exists in `sandboxprofile/env.go`; a future `checkEnvVars` category could lint `environment.allow_vars` against it.
- **Workdir filesystem scan** — deferred; would require touching the workdir at audit time to detect actual `.env`/`*.key` files under a broad grant. Static-only for now.
- **`--severity` threshold flag** — deferred; current exit code is hardcoded to fail on HIGH only. If CI wants to fail on MEDIUM, add `--fail-on medium` later.
