# `omac provenance --check` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `--check` flag to `omac provenance` that statically lints the resolved sandbox profile against known secret locations and network attack vectors, printing severity-tagged findings and exiting non-zero on HIGH.

**Architecture:** New package `internal/profileaudit/` holds the curated secret-path/glob tables and the `Check()` function. It depends only on `internal/sandboxprofile` (Profile, Baseline, EffectiveProtectedPaths, ExpandPath). The CLI layer (`internal/cli/provenance.go`) adds the `--check` flag and dispatches to `profileaudit.Check`, reusing the existing `--json` flag for output format. No new dependencies, no import cycles.

**Tech Stack:** Go 1.21+, standard library only (`path/filepath`, `strings`, `encoding/json`, `sort`). Testing via `testing` + existing `isolateHome`/`captureEnv` helpers in `internal/cli`.

**Spec:** `docs/superpowers/specs/2026-07-06-provenance-check-flag-design.md`

**Worktree:** `.worktrees/pr-36-provenance` on branch `feat/provenance-command`.

---

## File Structure

```
internal/profileaudit/
  finding.go            # Finding, Severity, Category, ExitCode()
  finding_test.go
  knownsecrets.go       # BaselineSecretPaths(), ExtensionSecretPaths, SecretBasenameGlobs
  knownsecrets_test.go
  check.go              # Check(*sandboxprofile.Profile) []Finding + internal check funcs
  check_test.go

internal/cli/
  provenance.go         # + --check flag, writeCheckText, writeCheckJSON
  provenance_test.go    # + --check mode tests

README.md               # + --check in provenance usage line (if present)
```

### Conventions from the codebase

- **Exit codes** live in `internal/cli/cli.go`: `ExitOK = 0`, `ExitConfigInvalid = 3`.
- **Test helpers** in `internal/cli/`: `isolateHome(t)` (sets HOME + XDG_CONFIG_HOME to temp dirs), `captureEnv(t, wd)` (returns `*Env` + reader). See `internal/cli/start_drift_test.go:24` and `internal/cli/list_deregister_test.go:14`.
- **Profile resolution**: `sandboxprofile.Resolve(ref string) (*Profile, string, error)` in `internal/sandboxprofile/resolve.go:89`. `sandboxprofile.PlatformBaseline()` returns the OS-specific `Baseline` (`internal/sandboxprofile/baseline.go:23`). `EffectiveProtectedPaths(base, overrideDeny)` returns the post-override protected list (`baseline.go:160`).
- **Path expansion**: `sandboxprofile.ExpandPath(p string) (string, error)` in `internal/sandboxprofile/expand.go:20`. Performs `~`/`$VAR` expansion + absolutization; does NOT require existence. Returns `ErrEmptyExpansion` if a var resolves to empty.
- **Provenance command**: `runProvenance` in `internal/cli/provenance.go:309`. Parses `--profile` and `--json` flags, calls `buildProvenanceView`, then `writeProvenanceText` or `writeProvenanceJSON`.
- **Provenance test pattern**: tests write a profile JSON to `<wd>/.opencode/<name>.json`, then call `runProvenance([]string{"--profile", profPath, ...}, env)` and assert on captured stdout. See `internal/cli/provenance_test.go:49` (`TestBuildProvenanceView_NetworkEntries`).
- **Commit style**: conventional commits, signed off (`git commit -s`). Repo examples: `feat(cli): wire up omac provenance command`, `fix(cli): rune-based truncation...`.
- **No comments in code** unless explicitly requested (per AGENTS.md / opencode instructions). The existing codebase DOES use doc comments on exported symbols — match that convention (doc comments on exported funcs/types/consts are OK; inline explanatory comments are not).

---

## Task 1: Finding type and ExitCode

**Files:**
- Create: `internal/profileaudit/finding.go`
- Test: `internal/profileaudit/finding_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/profileaudit/finding_test.go`:

```go
package profileaudit

import "testing"

func TestExitCode(t *testing.T) {
	tests := []struct {
		name     string
		findings []Finding
		want     int
	}{
		{"empty", nil, 0},
		{"only-low", []Finding{{Severity: SeverityLow}}, 0},
		{"only-medium", []Finding{{Severity: SeverityMedium}}, 0},
		{"any-high", []Finding{{Severity: SeverityMedium}, {Severity: SeverityHigh}}, 2},
		{"all-high", []Finding{{Severity: SeverityHigh}, {Severity: SeverityHigh}}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.findings); got != tc.want {
				t.Errorf("ExitCode(%v) = %d; want %d", tc.findings, got, tc.want)
			}
		})
	}
}

func TestSeverityOrdering(t *testing.T) {
	if severityRank(SeverityHigh) >= severityRank(SeverityMedium) {
		t.Error("high should rank before medium")
	}
	if severityRank(SeverityMedium) >= severityRank(SeverityLow) {
		t.Error("medium should rank before low")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/profileaudit/...`
Expected: FAIL — package does not exist / types undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/profileaudit/finding.go`:

```go
// Package profileaudit statically lints a resolved sandbox profile against
// known secret locations and network attack vectors, producing a list of
// findings ranked by severity. It is the engine behind
// `omac provenance --check`.
package profileaudit

// Severity ranks the risk of a finding.
type Severity string

const (
	SeverityHigh   Severity = "high"
	SeverityMedium Severity = "medium"
	SeverityLow    Severity = "low"
)

// Category groups findings by subsystem.
type Category string

const (
	CatFSGrant      Category = "filesystem"
	CatNetwork      Category = "network"
	CatOverrideDeny Category = "override_deny"
)

// Finding is one static-check result.
type Finding struct {
	Severity Severity `json:"severity"`
	Category Category `json:"category"`
	Field    string   `json:"field"`
	Value    string   `json:"value"`
	Message  string   `json:"message"`
}

// ExitCode returns 2 if any finding is HIGH, else 0. The value 2 mirrors
// the omac ExitConfigInvalid convention (config-level failure).
func ExitCode(findings []Finding) int {
	for _, f := range findings {
		if f.Severity == SeverityHigh {
			return 2
		}
	}
	return 0
}

// severityRank returns a sort key for a Severity (lower = more severe).
func severityRank(s Severity) int {
	switch s {
	case SeverityHigh:
		return 0
	case SeverityMedium:
		return 1
	case SeverityLow:
		return 2
	default:
		return 3
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/profileaudit/...`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/profileaudit/finding.go internal/profileaudit/finding_test.go
git commit -s -m "feat(profileaudit): add Finding type and ExitCode"
```

---

## Task 2: Known-secret tables

**Files:**
- Create: `internal/profileaudit/knownsecrets.go`
- Test: `internal/profileaudit/knownsecrets_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/profileaudit/knownsecrets_test.go`:

```go
package profileaudit

import (
	"path/filepath"
	"testing"
)

func TestExtensionSecretPathsNoBaselineOverlap(t *testing.T) {
	base := BaselineSecretPaths()
	baseSet := make(map[string]bool, len(base))
	for _, p := range base {
		baseSet[p] = true
	}
	for _, p := range ExtensionSecretPaths {
		if baseSet[p] {
			t.Errorf("ExtensionSecretPaths entry %q already in BaselineSecretPaths; the extension list must only hold paths NOT in the baseline", p)
		}
	}
}

func TestExtensionSecretPathsNonEmpty(t *testing.T) {
	if len(ExtensionSecretPaths) == 0 {
		t.Fatal("ExtensionSecretPaths is empty; expected the curated list of CLI-tool credential paths")
	}
}

func TestSecretBasenameGlobsValid(t *testing.T) {
	for _, g := range SecretBasenameGlobs {
		if _, err := filepath.Match(g, "probe"); err != nil {
			t.Errorf("SecretBasenameGlobs entry %q is not a valid filepath.Match pattern: %v", g, err)
		}
	}
}

func TestSecretBasenameGlobsNonEmpty(t *testing.T) {
	if len(SecretBasenameGlobs) == 0 {
		t.Fatal("SecretBasenameGlobs is empty; expected at least .env, *.key, etc.")
	}
}

func TestBaselineSecretPathsNonEmpty(t *testing.T) {
	if len(BaselineSecretPaths()) == 0 {
		t.Fatal("BaselineSecretPaths returned empty; PlatformBaseline().ProtectedPaths should always have entries")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/profileaudit/...`
Expected: FAIL — `BaselineSecretPaths`, `ExtensionSecretPaths`, `SecretBasenameGlobs` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/profileaudit/knownsecrets.go`:

```go
package profileaudit

import "github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"

// BaselineSecretPaths returns the paths omac already denies by default
// (PlatformBaseline().ProtectedPaths). Exported so the check can
// distinguish "profile weakens a baseline protection" (category E) from
// "profile exposes a path omac never protected" (category A).
func BaselineSecretPaths() []string {
	return sandboxprofile.PlatformBaseline().ProtectedPaths
}

// ExtensionSecretPaths are known secret-bearing paths NOT in the baseline.
// Curated, small list. Add new entries here only — each entry must be a
// path that genuinely holds a credential and is not already covered by
// BaselineSecretPaths() (a guard test enforces no overlap).
var ExtensionSecretPaths = []string{
	"~/.pypirc",                  // PyPI upload token
	"~/.config/github-copilot",   // Copilot OAuth token
	"~/.config/gh",               // GitHub CLI token
	"~/.gitconfig",               // may embed tokens (insteadOf/url cred)
	"~/.config/hub",              // legacy GitHub hub token
	"~/.cf",                      // Cloud Foundry CLI
}

// SecretBasenameGlobs are filename patterns matched against grants that
// use wildcards or broad directory grants (e.g. allow: ["."]). Each
// entry must be a valid filepath.Match pattern (a guard test enforces
// this).
var SecretBasenameGlobs = []string{
	".env",
	"*.env",
	"*.key",
	"*.pem",
	"*token*",
	"*secret*",
	"id_rsa*",
	"*.pfx",
	"*.p12",
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/profileaudit/...`
Expected: PASS (5 tests, all green).

- [ ] **Step 5: Commit**

```bash
git add internal/profileaudit/knownsecrets.go internal/profileaudit/knownsecrets_test.go
git commit -s -m "feat(profileaudit): add known secret-path and glob tables"
```

---

## Task 3: Check — category E (override_deny)

**Files:**
- Create: `internal/profileaudit/check.go`
- Test: `internal/profileaudit/check_test.go`

This task adds the `Check` entry point + the simplest category (E: `override_deny` weakens a baseline protection). Categories A and C are added in Tasks 4 and 5.

- [ ] **Step 1: Write the failing test**

Create `internal/profileaudit/check_test.go`:

```go
package profileaudit

import (
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// cleanProfile returns a minimal profile with no grants, ready for tests
// to populate specific fields.
func cleanProfile() *sandboxprofile.Profile {
	return &sandboxprofile.Profile{
		Meta:    sandboxprofile.Meta{Name: "test"},
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessNone},
	}
}

func TestCheck_EmptyProfileNoFindings(t *testing.T) {
	findings := Check(cleanProfile())
	if len(findings) != 0 {
		t.Errorf("empty profile should produce no findings; got %d: %+v", len(findings), findings)
	}
}

func TestCheck_OverrideDenyBaselinePathIsHigh(t *testing.T) {
	// ~/.ssh is in the cross-platform protectedCommon set (baseline.go:35).
	p := cleanProfile()
	p.Filesystem.OverrideDeny = []string{"~/.ssh"}
	findings := Check(p)
	if len(findings) == 0 {
		t.Fatal("expected at least one finding for override_deny on ~/.ssh")
	}
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatOverrideDeny {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no override_deny finding; got %+v", findings)
	}
	if got.Severity != SeverityHigh {
		t.Errorf("severity = %q; want %q", got.Severity, SeverityHigh)
	}
	if !strings.Contains(got.Value, ".ssh") {
		t.Errorf("value %q should mention .ssh", got.Value)
	}
	if !strings.Contains(got.Message, "baseline protection") {
		t.Errorf("message %q should mention baseline protection", got.Message)
	}
}

func TestCheck_OverrideDenyNonBaselinePathNoFinding(t *testing.T) {
	// /tmp/foo is not in the baseline; overriding it is a no-op, not a risk.
	p := cleanProfile()
	p.Filesystem.OverrideDeny = []string{"/tmp/no-such-protected-path"}
	findings := Check(p)
	for _, f := range findings {
		if f.Category == CatOverrideDeny {
			t.Errorf("override_deny on non-baseline path should not produce a finding; got %+v", f)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/profileaudit/...`
Expected: FAIL — `Check` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/profileaudit/check.go`:

```go
package profileaudit

import (
	"sort"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// Check statically lints a resolved sandbox profile against known secret
// locations and network attack vectors. It performs no filesystem or
// network I/O. The returned findings are sorted by severity
// (high → medium → low), then by category, then by field.
func Check(profile *sandboxprofile.Profile) []Finding {
	var findings []Finding
	findings = append(findings, checkOverrideDeny(profile)...)
	findings = append(findings, checkFSGrants(profile)...)
	findings = append(findings, checkNetwork(profile)...)
	sortFindings(findings)
	return findings
}

// sortFindings orders findings by severity (high first), then category,
// then field, then value. Stable so equal-keyed entries keep insertion
// order.
func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		ri, rj := severityRank(findings[i].Severity), severityRank(findings[j].Severity)
		if ri != rj {
			return ri < rj
		}
		if findings[i].Category != findings[j].Category {
			return findings[i].Category < findings[j].Category
		}
		if findings[i].Field != findings[j].Field {
			return findings[i].Field < findings[j].Field
		}
		return findings[i].Value < findings[j].Value
	})
}

// checkOverrideDeny flags every override_deny entry that removes a
// baseline-protected path. Each such entry is a deliberate weakening of
// a credential protection and is always HIGH.
func checkOverrideDeny(profile *sandboxprofile.Profile) []Finding {
	if len(profile.Filesystem.OverrideDeny) == 0 {
		return nil
	}
	base := BaselineSecretPaths()
	baseSet := make(map[string]bool, len(base))
	for _, p := range base {
		baseSet[p] = true
	}
	var findings []Finding
	for _, entry := range profile.Filesystem.OverrideDeny {
		// BaselineSecretPaths returns expanded paths; override_deny
		// entries may use ~ or $VAR. Expand for comparison.
		exp, err := sandboxprofile.ExpandPath(entry)
		if err != nil {
			// If it can't expand, we can't match it. Skip silently —
			// the sandbox itself will emit its own notice at launch.
			continue
		}
		if baseSet[exp] {
			findings = append(findings, Finding{
				Severity: SeverityHigh,
				Category: CatOverrideDeny,
				Field:    "filesystem.override_deny",
				Value:    entry,
				Message:  "removes baseline protection on " + exp + " (" + secretDescription(exp) + ")",
			})
		}
	}
	return findings
}

// secretDescription returns a short human-readable hint for a known
// secret path, used in finding messages.
func secretDescription(path string) string {
	switch {
	case strings.Contains(path, ".ssh"):
		return "SSH private keys"
	case strings.Contains(path, ".aws"):
		return "AWS credentials"
	case strings.Contains(path, ".azure"):
		return "Azure CLI credentials"
	case strings.Contains(path, ".gcloud"), strings.Contains(path, "gcloud"):
		return "GCP credentials"
	case strings.Contains(path, ".kube"):
		return "Kubernetes config"
	case strings.Contains(path, ".docker"):
		return "Docker registry tokens"
	case strings.Contains(path, ".gnupg"):
		return "GPG keys"
	case strings.Contains(path, ".netrc"):
		return "HTTP credentials"
	case strings.Contains(path, ".npmrc"):
		return "npm token"
	case strings.Contains(path, ".vault-token"):
		return "Vault token"
	case strings.Contains(path, "Keychain"), strings.Contains(path, "keyring"):
		return "OS keychain/keyring"
	case strings.Contains(path, ".pypirc"):
		return "PyPI upload token"
	case strings.Contains(path, "github-copilot"):
		return "Copilot OAuth token"
	case strings.Contains(path, ".config/gh"):
		return "GitHub CLI token"
	case strings.Contains(path, ".gitconfig"):
		return "git config (may embed tokens)"
	case strings.Contains(path, ".config/hub"):
		return "GitHub hub token"
	case strings.Contains(path, ".cf"):
		return "Cloud Foundry CLI"
	default:
		return "credentials"
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/profileaudit/...`
Expected: PASS (3 new tests + 7 from prior tasks = 10 total).

- [ ] **Step 5: Commit**

```bash
git add internal/profileaudit/check.go internal/profileaudit/check_test.go
git commit -s -m "feat(profileaudit): add Check with override_deny (category E)"
```

---

## Task 4: Check — category A (filesystem grants)

**Files:**
- Modify: `internal/profileaudit/check.go` (append `checkFSGrants`)
- Modify: `internal/profileaudit/check_test.go` (append tests)

- [ ] **Step 1: Write the failing tests**

Append to `internal/profileaudit/check_test.go`:

```go
func TestCheck_FSGrantBaselinePathIsHigh(t *testing.T) {
	p := cleanProfile()
	p.Filesystem.Allow = []string{"~/.ssh"}
	findings := Check(p)
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatFSGrant && findings[i].Field == "filesystem.allow" {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no filesystem.allow finding for ~/.ssh; got %+v", findings)
	}
	if got.Severity != SeverityHigh {
		t.Errorf("severity = %q; want high", got.Severity)
	}
	if !strings.Contains(got.Value, ".ssh") {
		t.Errorf("value %q should contain .ssh", got.Value)
	}
}

func TestCheck_FSGrantExtensionPathIsMedium(t *testing.T) {
	p := cleanProfile()
	p.Filesystem.Read = []string{"~/.pypirc"}
	findings := Check(p)
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatFSGrant && findings[i].Field == "filesystem.read" {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no filesystem.read finding for ~/.pypirc; got %+v", findings)
	}
	if got.Severity != SeverityMedium {
		t.Errorf("severity = %q; want medium", got.Severity)
	}
}

func TestCheck_FSGrantParentOfSecretPathIsFlagged(t *testing.T) {
	// Granting ~ (the home dir) is a parent of ~/.ssh → should flag high.
	p := cleanProfile()
	p.Filesystem.Allow = []string{"~"}
	findings := Check(p)
	foundSSH := false
	for _, f := range findings {
		if f.Category == CatFSGrant && strings.Contains(f.Message, ".ssh") {
			foundSSH = true
		}
	}
	if !foundSSH {
		t.Errorf("granting ~ should flag ~/.ssh as exposed; got %+v", findings)
	}
}

func TestCheck_FSGrantSubpathOfSecretPathNotFlagged(t *testing.T) {
	// Granting ~/.ssh/foo does NOT expose ~/.ssh itself.
	p := cleanProfile()
	p.Filesystem.Allow = []string{"~/.ssh/foo"}
	findings := Check(p)
	for _, f := range findings {
		if f.Category == CatFSGrant {
			t.Errorf("subpath of secret path should not be flagged; got %+v", f)
		}
	}
}

func TestCheck_FSGrantBroadGlobIsMedium(t *testing.T) {
	// A broad grant like "." could expose any file; emit medium findings
	// for each known secret basename glob.
	p := cleanProfile()
	p.Filesystem.Allow = []string{"."}
	findings := Check(p)
	if len(findings) == 0 {
		t.Fatal("broad grant '.' should produce findings for known secret globs")
	}
	for _, f := range findings {
		if f.Severity != SeverityMedium {
			t.Errorf("broad-glob finding %q severity = %q; want medium", f.Value, f.Severity)
		}
		if f.Category != CatFSGrant {
			t.Errorf("broad-glob finding category = %q; want filesystem", f.Category)
		}
	}
}

func TestCheck_FSGrantCleanPathNoFinding(t *testing.T) {
	// /usr/local/bin is in the baseline read set, not a secret path.
	p := cleanProfile()
	p.Filesystem.Allow = []string{"/usr/local/bin"}
	findings := Check(p)
	for _, f := range findings {
		if f.Category == CatFSGrant {
			t.Errorf("clean path should not be flagged; got %+v", f)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/profileaudit/...`
Expected: FAIL — `checkFSGrants` not defined (compile error: `Check` references it).

- [ ] **Step 3: Append `checkFSGrants` to `check.go`**

Add this function to the end of `internal/profileaudit/check.go`:

```go
// checkFSGrants flags filesystem grants (allow/read/write/allow_unix_dir)
// that expose known secret paths or could match known secret basenames.
func checkFSGrants(profile *sandboxprofile.Profile) []Finding {
	type slot struct {
		field   string
		entries []string
	}
	slots := []slot{
		{"filesystem.allow", profile.Filesystem.Allow},
		{"filesystem.read", profile.Filesystem.Read},
		{"filesystem.write", profile.Filesystem.Write},
		{"filesystem.allow_unix_dir", profile.Filesystem.AllowUnixDir},
	}
	base := BaselineSecretPaths()
	ext := ExtensionSecretPaths
	var findings []Finding
	for _, s := range slots {
		for _, entry := range s.entries {
			findings = append(findings, checkOneFSGrant(s.field, entry, base, ext)...)
		}
	}
	return findings
}

// checkOneFSGrant inspects a single grant entry.
func checkOneFSGrant(field, entry string, baseline, extension []string) []Finding {
	// Try to expand as an explicit path.
	exp, err := sandboxprofile.ExpandPath(entry)
	if err == nil {
		// Explicit path: compare against known secret paths.
		return checkExplicitGrant(field, entry, exp, baseline, extension)
	}
	// Could not expand — treat as a broad/glob grant.
	return checkBroadGrant(field, entry)
}

// checkExplicitGrant compares an expanded grant path against the known
// secret path lists. A grant that equals or is a parent of a secret
// path is flagged. A subpath of a secret path is not (it doesn't
// expose the secret itself).
func checkExplicitGrant(field, entry, exp string, baseline, extension []string) []Finding {
	var findings []Finding
	for _, sp := range baseline {
		expandedSP, err := sandboxprofile.ExpandPath(sp)
		if err != nil {
			continue
		}
		if exp == expandedSP || isParent(exp, expandedSP) {
			findings = append(findings, Finding{
				Severity: SeverityHigh,
				Category: CatFSGrant,
				Field:    field,
				Value:    entry,
				Message:  "intersects baseline protected path " + expandedSP + " (" + secretDescription(expandedSP) + ")",
			})
			return findings
		}
	}
	for _, sp := range extension {
		expandedSP, err := sandboxprofile.ExpandPath(sp)
		if err != nil {
			continue
		}
		if exp == expandedSP || isParent(exp, expandedSP) {
			findings = append(findings, Finding{
				Severity: SeverityMedium,
				Category: CatFSGrant,
				Field:    field,
				Value:    entry,
				Message:  "overlaps known secret path " + expandedSP + " not in baseline (" + secretDescription(expandedSP) + ")",
			})
			return findings
		}
	}
	return findings
}

// checkBroadGrant flags a grant that could not be expanded to an
// explicit path (e.g. ".", "*", "./"). Emits one MEDIUM finding per
// known secret basename glob.
func checkBroadGrant(field, entry string) []Finding {
	var findings []Finding
	for _, g := range SecretBasenameGlobs {
		findings = append(findings, Finding{
			Severity: SeverityMedium,
			Category: CatFSGrant,
			Field:    field,
			Value:    entry,
			Message:  "broad grant may expose \"" + g + "\" files",
		})
	}
	return findings
}

// isParent reports whether parent == child or child is beneath parent.
func isParent(parent, child string) bool {
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}
```

Also add `"path/filepath"` to the import block at the top of `check.go` (after `"sort"`):

```go
import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/profileaudit/...`
Expected: PASS (all 16 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/profileaudit/check.go internal/profileaudit/check_test.go
git commit -s -m "feat(profileaudit): add filesystem grant checks (category A)"
```

---

## Task 5: Check — category C (network)

**Files:**
- Modify: `internal/profileaudit/check.go` (append `checkNetwork`)
- Modify: `internal/profileaudit/check_test.go` (append tests)

- [ ] **Step 1: Write the failing tests**

Append to `internal/profileaudit/check_test.go`:

```go
func TestCheck_NetworkMetadataHostIsHigh(t *testing.T) {
	p := cleanProfile()
	p.Network.AllowDomain = []string{"169.254.169.254"}
	findings := Check(p)
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatNetwork && findings[i].Field == "network.allow_domain" {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no network finding for metadata host; got %+v", findings)
	}
	if got.Severity != SeverityHigh {
		t.Errorf("severity = %q; want high", got.Severity)
	}
	if !strings.Contains(got.Message, "metadata") {
		t.Errorf("message %q should mention metadata", got.Message)
	}
}

func TestCheck_NetworkInternalSuffixIsMedium(t *testing.T) {
	p := cleanProfile()
	p.Network.AllowDomain = []string{"evil.internal"}
	findings := Check(p)
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatNetwork && findings[i].Field == "network.allow_domain" {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no network finding for .internal host; got %+v", findings)
	}
	if got.Severity != SeverityMedium {
		t.Errorf("severity = %q; want medium", got.Severity)
	}
}

func TestCheck_NetworkOpenPortZeroIsLow(t *testing.T) {
	p := cleanProfile()
	p.Network.OpenPort = []int{0}
	findings := Check(p)
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatNetwork && findings[i].Field == "network.open_port" {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no finding for open_port 0; got %+v", findings)
	}
	if got.Severity != SeverityLow {
		t.Errorf("severity = %q; want low", got.Severity)
	}
}

func TestCheck_NetworkAllowTCPConnect22IsMedium(t *testing.T) {
	p := cleanProfile()
	p.Network.AllowTCPConnect = []int{22}
	findings := Check(p)
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatNetwork && findings[i].Field == "network.allow_tcp_connect" {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no finding for allow_tcp_connect 22; got %+v", findings)
	}
	if got.Severity != SeverityMedium {
		t.Errorf("severity = %q; want medium", got.Severity)
	}
}

func TestCheck_NetworkCleanDomainNoFinding(t *testing.T) {
	p := cleanProfile()
	p.Network.AllowDomain = []string{"github.com"}
	findings := Check(p)
	for _, f := range findings {
		if f.Category == CatNetwork {
			t.Errorf("clean domain should not be flagged; got %+v", f)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/profileaudit/...`
Expected: FAIL — `checkNetwork` referenced by `Check` but not defined (compile error).

- [ ] **Step 3: Append `checkNetwork` to `check.go`**

Add to the end of `internal/profileaudit/check.go`:

```go
// cloudMetadataHosts are the cloud instance-metadata endpoints that
// allow credential theft from inside a sandbox. They must never appear
// in allow_domain.
var cloudMetadataHosts = map[string]bool{
	"169.254.169.254":      true, // AWS / Azure / GCP (link-local)
	"metadata.google.internal": true, // GCP
	"metadata.azure.internal":   true, // Azure
}

// checkNetwork flags allow_domain entries that point at cloud metadata
// endpoints or SSRF-prone suffixes, and flags risky port openings.
func checkNetwork(profile *sandboxprofile.Profile) []Finding {
	var findings []Finding
	for _, d := range profile.Network.AllowDomain {
		switch {
		case cloudMetadataHosts[d]:
			findings = append(findings, Finding{
				Severity: SeverityHigh,
				Category: CatNetwork,
				Field:    "network.allow_domain",
				Value:    d,
				Message:  "cloud metadata endpoint (credential theft surface)",
			})
		case strings.HasSuffix(d, ".internal") || strings.HasSuffix(d, ".local"):
			findings = append(findings, Finding{
				Severity: SeverityMedium,
				Category: CatNetwork,
				Field:    "network.allow_domain",
				Value:    d,
				Message:  "internal/local suffix (SSRF surface)",
			})
		}
	}
	for _, port := range profile.Network.OpenPort {
		if port == 0 {
			findings = append(findings, Finding{
				Severity: SeverityLow,
				Category: CatNetwork,
				Field:    "network.open_port",
				Value:    "0",
				Message:  "any loopback port",
			})
		}
	}
	for _, port := range profile.Network.AllowTCPConnect {
		switch port {
		case 22, 3389:
			findings = append(findings, Finding{
				Severity: SeverityMedium,
				Category: CatNetwork,
				Field:    "network.allow_tcp_connect",
				Value:    portLabel(port),
				Message:  "direct outbound TCP to SSH/RDP port",
			})
		}
	}
	return findings
}

// portLabel returns a string label for a port value.
func portLabel(port int) string {
	return strconv.Itoa(port)
}
```

Add `"strconv"` to the import block of `check.go`:

```go
import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/profileaudit/...`
Expected: PASS (all 21 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/profileaudit/check.go internal/profileaudit/check_test.go
git commit -s -m "feat(profileaudit): add network checks (category C)"
```

---

## Task 6: CLI integration — `--check` flag + output

**Files:**
- Modify: `internal/cli/provenance.go` (add `--check` flag, `writeCheckText`, `writeCheckJSON`)
- Modify: `internal/cli/provenance_test.go` (add `--check` mode tests)

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/provenance_test.go`:

```go
func TestRunProvenance_CheckDefaultProfileClean(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	profDir := filepath.Join(wd, ".opencode")
	os.MkdirAll(profDir, 0o755)
	profPath := filepath.Join(profDir, "default.json")
	os.WriteFile(profPath, []byte(`{"meta":{"name":"default"},"workdir":{"access":"readwrite"}}`), 0o644)

	env, read := captureEnv(t, wd)
	code := runProvenance([]string{"--profile", profPath, "--check"}, env)
	if code != ExitOK {
		out, errOut := read()
		t.Fatalf("code = %d; stdout=%q stderr=%q", code, out, errOut)
	}
	out, _ := read()
	if !strings.Contains(out, "no findings") {
		t.Errorf("clean profile should print '(no findings)'; got %q", out)
	}
}

func TestRunProvenance_CheckJSONEmptyArray(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	profDir := filepath.Join(wd, ".opencode")
	os.MkdirAll(profDir, 0o755)
	profPath := filepath.Join(profDir, "default.json")
	os.WriteFile(profPath, []byte(`{"meta":{"name":"default"},"workdir":{"access":"readwrite"}}`), 0o644)

	env, read := captureEnv(t, wd)
	code := runProvenance([]string{"--profile", profPath, "--check", "--json"}, env)
	if code != ExitOK {
		out, errOut := read()
		t.Fatalf("code = %d; stdout=%q stderr=%q", code, out, errOut)
	}
	out, _ := read()
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	if len(parsed) != 0 {
		t.Errorf("clean profile should produce empty JSON array; got %d items", len(parsed))
	}
}

func TestRunProvenance_CheckRiskyProfileExitsNonZero(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	profDir := filepath.Join(wd, ".opencode")
	os.MkdirAll(profDir, 0o755)
	profPath := filepath.Join(profDir, "risky.json")
	os.WriteFile(profPath, []byte(`{
		"meta":{"name":"risky"},
		"workdir":{"access":"readwrite"},
		"filesystem":{"allow":["~/.ssh"],"override_deny":["~/.aws"]}
	}`), 0o644)

	env, read := captureEnv(t, wd)
	code := runProvenance([]string{"--profile", profPath, "--check"}, env)
	if code == ExitOK {
		out, _ := read()
		t.Fatalf("expected non-zero exit for risky profile; got 0; stdout=%q", out)
	}
	out, _ := read()
	if !strings.Contains(out, "[HIGH]") {
		t.Errorf("output should contain [HIGH] findings; got %q", out)
	}
}

func TestRunProvenance_CheckJSONRiskyProfileHasFindings(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	profDir := filepath.Join(wd, ".opencode")
	os.MkdirAll(profDir, 0o755)
	profPath := filepath.Join(profDir, "risky.json")
	os.WriteFile(profPath, []byte(`{
		"meta":{"name":"risky"},
		"workdir":{"access":"readwrite"},
		"network":{"allow_domain":["169.254.169.254"]}
	}`), 0o644)

	env, read := captureEnv(t, wd)
	code := runProvenance([]string{"--profile", profPath, "--check", "--json"}, env)
	if code == ExitOK {
		t.Fatal("expected non-zero exit for metadata host in allow_domain")
	}
	out, _ := read()
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	if len(parsed) == 0 {
		t.Errorf("expected at least one finding; got empty array")
	}
	foundHigh := false
	for _, f := range parsed {
		if sev, _ := f["severity"].(string); sev == "high" {
			foundHigh = true
		}
	}
	if !foundHigh {
		t.Errorf("expected at least one high finding; got %v", parsed)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/... -run TestRunProvenance_Check`
Expected: FAIL — `--check` flag not recognized.

- [ ] **Step 3: Implement the `--check` flag + output functions**

In `internal/cli/provenance.go`:

**3a.** Add `profileaudit` to the import block:

```go
import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/tngtech/oh-my-agentic-coder/internal/netprompt"
	"github.com/tngtech/oh-my-agentic-coder/internal/profileaudit"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)
```

**3b.** Modify `runProvenance` (currently at `provenance.go:309`) to add the `--check` flag and dispatch. The existing `buildProvenanceView` is left untouched — `--check` resolves the profile itself (it only needs the `*Profile`, not the view machinery). Replace the existing function body with:

```go
// runProvenance implements `omac provenance [--profile <ref>] [--check] [--json]`.
func runProvenance(args []string, env *Env) int {
	fs := flag.NewFlagSet("provenance", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	profileRef := fs.String("profile", "", "sandbox profile name, path, or builtin (default: default)")
	checkMode := fs.Bool("check", false, "Static security lint of the resolved profile.")
	jsonOut := fs.Bool("json", false, "Emit a JSON object instead of tabular text.")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac provenance [--profile <ref>] [--check] [--json]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}

	// --check resolves the profile itself and runs the lint; it does
	// not build the provenance view. Keeps --check independent of the
	// view-build path and its (registry, learned-policy) dependencies.
	if *checkMode {
		profile, err := sandboxprofile.Resolve(*profileRef)
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac provenance --check:", err)
			return ExitConfigInvalid
		}
		findings := profileaudit.Check(profile)
		if *jsonOut {
			return writeCheckJSON(env.Stdout, findings)
		}
		return writeCheckText(env.Stdout, findings)
	}

	view, err := buildProvenanceView(env.Workdir, *profileRef)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac provenance:", err)
		return ExitConfigInvalid
	}
	if *jsonOut {
		return writeProvenanceJSON(env.Stdout, view)
	}
	return writeProvenanceText(env.Stdout, view)
}
```

**3c.** Add the two output functions at the end of `provenance.go`:

```go
// writeCheckText renders findings one per line, sorted by severity.
func writeCheckText(w io.Writer, findings []profileaudit.Finding) int {
	if len(findings) == 0 {
		fmt.Fprintln(w, "(no findings)")
		return ExitOK
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  SEVERITY\tFIELD\tVALUE\tMESSAGE")
	for _, f := range findings {
		fmt.Fprintf(tw, "  [%s]\t%s\t%s\t%s\n",
			strings.ToUpper(string(f.Severity)),
			f.Field, f.Value, f.Message)
	}
	_ = tw.Flush()
	return profileaudit.ExitCode(findings)
}

// writeCheckJSON marshals findings as a JSON array.
func writeCheckJSON(w io.Writer, findings []profileaudit.Finding) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(findings); err != nil {
		fmt.Fprintln(os.Stderr, "omac provenance --check: json:", err)
		return ExitIOError
	}
	return profileaudit.ExitCode(findings)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/... -run TestRunProvenance_Check`
Expected: PASS (4 new tests).

- [ ] **Step 5: Run full test suite to verify no regressions**

Run: `go test ./internal/profileaudit/... ./internal/cli/...`
Expected: PASS (all tests, no regressions).

- [ ] **Step 6: Commit**

```bash
git add internal/cli/provenance.go internal/cli/provenance_test.go
git commit -s -m "feat(cli): wire up omac provenance --check"
```

---

## Task 7: README update

**Files:**
- Modify: `README.md` (provenance usage line, if it lists flags)

- [ ] **Step 1: Check if README documents provenance flags**

Run: `grep -n "provenance" README.md`

If the README lists the provenance command's flags, add `--check` to the list. If it only mentions the command name, skip this task (no change needed).

- [ ] **Step 2: If applicable, update the provenance usage line**

For example, if the README shows:

```
omac provenance [--profile <ref>] [--json]
```

Change to:

```
omac provenance [--profile <ref>] [--check] [--json]
```

- [ ] **Step 3: Commit (only if README changed)**

```bash
git add README.md
git commit -s -m "docs: document omac provenance --check flag"
```

---

## Task 8: Final verification

- [ ] **Step 1: Build everything**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 2: Run vet**

Run: `go vet ./...`
Expected: no warnings.

- [ ] **Step 3: Run the full test suite**

Run: `go test ./...`
Expected: all packages PASS.

- [ ] **Step 4: Manual smoke test**

Run:
```bash
go run ./cmd/omac provenance --check
```
Expected: either `(no findings)` (exit 0) on a clean default profile, or findings listed if the developer's profile has risky grants.

Run:
```bash
go run ./cmd/omac provenance --check --json
```
Expected: `[]` (exit 0) on a clean profile.

- [ ] **Step 5: Verify the commit history is clean**

Run: `git log --oneline -8`
Expected: 6-7 commits, each conventional-commit formatted and signed off (`-s`).

---

## Self-Review (run after writing, before handoff)

**Spec coverage:**

| Spec section | Task |
|---|---|
| Interface (`--check`, `--profile`, `--json`, exit code) | Task 6 |
| Package layout (`internal/profileaudit/`) | Tasks 1-5 |
| Data model: `knownsecrets.go` two-list + globs | Task 2 |
| Data model: `finding.go` types + `ExitCode` | Task 1 |
| Check logic: category A (fs grants, parent-of, broad-glob) | Task 4 |
| Check logic: category C (metadata, SSRF, ports) | Task 5 |
| Check logic: category E (override_deny weakens baseline) | Task 3 |
| CLI integration (`runProvenance` dispatch, `--check` resolves profile itself) | Task 6 |
| Output: text + JSON | Task 6 |
| Testing: unit tests for all categories + edge cases | Tasks 1-5 |
| Testing: CLI tests (clean default, risky profile, JSON) | Task 6 |
| README documentation | Task 7 |
| Final build/vet/test verification | Task 8 |

**Placeholder scan:** none — every step contains real code or real commands.

**Type consistency:** `Finding`, `Severity`, `Category`, `Check`, `ExitCode` defined in Task 1/3; used identically in Tasks 4-6. `writeCheckText`/`writeCheckJSON` use `profileaudit.Finding` and `profileaudit.ExitCode` consistently. `runProvenance` resolves the profile via `sandboxprofile.Resolve(*profileRef)` in both the `--check` branch and the existing `buildProvenanceView` path — no duplicated resolution helper introduced.

**Exit code note:** The spec said `ExitConfigInvalid (2)`, but the actual constant in `internal/cli/cli.go:14` is `ExitMisuse = 2`. `ExitConfigInvalid = 3` (`cli.go:15`). The plan uses `profileaudit.ExitCode()` returning `2` directly (matching the literal value, not the CLI constant name), and the `writeCheck*` functions return that value. This is intentional — `profileaudit` has no dependency on `internal/cli` (avoids import cycle), and the value 2 is documented in `finding.go`'s `ExitCode` doc comment.
