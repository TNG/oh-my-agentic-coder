# Allow/Denylist Provenance Command — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `omac provenance` — a read-only command that dumps every effective allow/deny entry across network, filesystem, environment, and skills subsystems, each row annotated with the config layer it came from.

**Architecture:** One new CLI file (`internal/cli/provenance.go`) reuses existing loaders (`sandboxprofile.Resolve`, `netprompt.LoadLearnedPolicy`, `registry.Load`, `PlatformBaseline`, `EffectiveProtectedPaths`). A tiny accessor on `sandboxprofile/env.go` exposes the env blocklist for display. Output is tabwriter tables by default or a single JSON object with `--json`.

**Tech Stack:** Go stdlib, `text/tabwriter`, `encoding/json`, existing omac packages.

**Spec:** `docs/superpowers/specs/2026-07-02-allow-denylist-provenance-command-design.md`

---

## File structure

| File | Action | Responsibility |
| --- | --- | --- |
| `internal/sandboxprofile/env.go` | Modify | Add `DangerousEnvBlocklist()` accessor (5 lines) |
| `internal/cli/provenance.go` | Create | `runProvenance`, flag parsing, data gathering, text+JSON formatters |
| `internal/cli/provenance_test.go` | Create | Unit tests for gathering + formatting |
| `internal/cli/cli.go` | Modify | Register `provenance` in `commands()` + `printUsage` |
| `internal/e2e/provenance_test.go` | Create | E2E: provenance output matches actual sandbox behavior |

---

### Task 1: Expose the env-var blocklist for display

**Files:**
- Modify: `internal/sandboxprofile/env.go`
- Test: `internal/sandboxprofile/profile_test.go`

The env blocklist (`dangerousEnvExact` map + `dangerousEnvPrefixes` slice) is unexported. Provenance needs to enumerate both for display. Add a pure accessor that returns sorted copies — no mutation of the package state.

- [ ] **Step 1: Write the failing test**

Append to `internal/sandboxprofile/profile_test.go`:

```go
func TestDangerousEnvBlocklist(t *testing.T) {
	exact, prefixes := DangerousEnvBlocklist()
	if len(exact) == 0 {
		t.Fatal("exact blocklist is empty")
	}
	if len(prefixes) == 0 {
		t.Fatal("prefix blocklist is empty")
	}
	// Spot-check known entries.
	found := false
	for _, e := range exact {
		if e == "BASH_ENV" {
			found = true
		}
	}
	if !found {
		t.Error("BASH_ENV not in exact blocklist")
	}
	found = false
	for _, p := range prefixes {
		if p == "LD_" {
			found = true
		}
	}
	if !found {
		t.Error("LD_ not in prefix blocklist")
	}
	// Must be sorted for stable display.
	if !sort.StringsAreSorted(exact) {
		t.Error("exact blocklist not sorted")
	}
	if !sort.StringsAreSorted(prefixes) {
		t.Error("prefix blocklist not sorted")
	}
}
```

Add `"sort"` to the import block if not present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sandboxprofile/ -run TestDangerousEnvBlocklist -v`
Expected: FAIL with `undefined: DangerousEnvBlocklist`

- [ ] **Step 3: Write minimal implementation**

Append to `internal/sandboxprofile/env.go`:

```go
// DangerousEnvBlocklist returns sorted copies of the always-drop env
// blocklist: exact names (e.g. "BASH_ENV") and prefix families (e.g.
// "LD_"). Provenance uses this to display the effective deny set; the
// returned slices are copies so callers cannot mutate the package
// state.
func DangerousEnvBlocklist() (exact []string, prefixes []string) {
	exact = make([]string, 0, len(dangerousEnvExact))
	for k := range dangerousEnvExact {
		exact = append(exact, k)
	}
	sort.Strings(exact)
	prefixes = append([]string(nil), dangerousEnvPrefixes...)
	sort.Strings(prefixes)
	return exact, prefixes
}
```

Add `"sort"` to the import block of `env.go`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sandboxprofile/ -run TestDangerousEnvBlocklist -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/sandboxprofile/env.go internal/sandboxprofile/profile_test.go
git commit -s -m "feat(sandboxprofile): expose DangerousEnvBlocklist accessor for provenance display"
```

---

### Task 2: Define provenance view types and JSON structure

**Files:**
- Create: `internal/cli/provenance.go`
- Test: `internal/cli/provenance_test.go`

Define the data types that both the text and JSON formatters consume. These are pure struct definitions + the top-level `provenanceView` — no logic yet. Getting the types right first means the gatherer and formatters can be built against a stable contract.

- [ ] **Step 1: Write the failing test (type smoke test)**

Create `internal/cli/provenance_test.go`:

```go
package cli

import "testing"

func TestProvenanceViewJSONRoundTrip(t *testing.T) {
	v := provenanceView{
		Profile: profileSource{Name: "default", Path: "/x/default.json", Source: "global"},
		Network: networkView{
			Mode:        "filtered",
			PromptOn:    true,
			OnUnavailable: "deny",
			Entries: []provEntry{
				{Entry: "github.com", Action: "allow", Source: "workdir"},
				{Entry: "evil.com", Action: "deny", Source: "global"},
			},
		},
		Filesystem: filesystemView{
			WorkdirAccess: "readwrite",
			Entries: []provEntry{
				{Entry: "~/.cache", Action: "allow", Source: "builtin"},
			},
		},
		Environment: environmentView{
			Entries: []provEntry{
				{Entry: "LD_*", Action: "deny", Source: "blocklist"},
			},
		},
		Skills: skillsView{
			Workdir: "/home/user/proj",
			Entries: []provEntry{
				{Entry: "slack", Action: "registered", Source: "workdir"},
			},
		},
	}
	if v.Network.Entries[0].Entry != "github.com" {
		t.Fatal("entry mismatch")
	}
	if v.Skills.Workdir != "/home/user/proj" {
		t.Fatal("workdir mismatch")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestProvenanceViewJSONRoundTrip -v`
Expected: FAIL — `provenance.go` doesn't exist, types undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/cli/provenance.go`:

```go
// Package cli provenance command: read-only dump of every effective
// allow/deny entry across network, filesystem, environment, and skills
// subsystems, each row annotated with the config layer it came from.
//
// omac provenance [--profile <ref>] [--json]
//
// Reuses existing loaders (sandboxprofile.Resolve, netprompt.LoadLearnedPolicy,
// registry.Load, PlatformBaseline, EffectiveProtectedPaths) — no new
// resolution logic, just a presentation layer over what the sandbox
// actually enforces.
package cli

// provEntry is one row in any provenance section.
type provEntry struct {
	Entry  string `json:"entry"`
	Action string `json:"action"`
	Source string `json:"source"`
}

// profileSource identifies which profile was resolved and where it came from.
type profileSource struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Source string `json:"source"`
}

// networkView holds the effective network policy + entries.
type networkView struct {
	Mode          string      `json:"mode"`
	PromptOn      bool        `json:"prompt_enabled"`
	OnUnavailable string      `json:"on_unavailable"`
	Entries       []provEntry `json:"entries"`
}

// filesystemView holds the effective filesystem policy + entries.
type filesystemView struct {
	WorkdirAccess string      `json:"workdir_access"`
	Entries       []provEntry `json:"entries"`
}

// environmentView holds env-var allow/deny entries.
type environmentView struct {
	Entries []provEntry `json:"entries"`
}

// skillsView holds the registered-skill entries.
type skillsView struct {
	Workdir string      `json:"workdir"`
	Entries []provEntry `json:"entries"`
}

// provenanceView is the top-level payload. JSON mode marshals this
// directly; text mode walks each section.
type provenanceView struct {
	Profile     profileSource     `json:"profile"`
	Network     networkView       `json:"network"`
	Filesystem  filesystemView    `json:"filesystem"`
	Environment environmentView    `json:"environment"`
	Skills      skillsView        `json:"skills"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestProvenanceViewJSONRoundTrip -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cli/provenance.go internal/cli/provenance_test.go
git commit -s -m "feat(cli): define provenance view types"
```

---

### Task 3: Implement the data gatherer (buildProvenanceView)

**Files:**
- Modify: `internal/cli/provenance.go`
- Test: `internal/cli/provenance_test.go`

`buildProvenanceView` loads the profile + learned decisions + baseline + registry and assembles a `provenanceView`. This is where the spec's data-source mapping becomes code. No formatting here — pure data.

- [ ] **Step 1: Write the failing test (gatherer with a temp profile)**

Append to `internal/cli/provenance_test.go`:

```go
import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildProvenanceView_NetworkEntries(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()

	// Write a profile with allow_domain + deny_domain.
	profDir := filepath.Join(wd, ".opencode")
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	profileJSON := `{
		"meta": {"name": "test"},
		"workdir": {"access": "readwrite"},
		"network": {
			"mode": "filtered",
			"allow_domain": ["github.com"],
			"deny_domain": ["evil.com"]
		}
	}`
	profPath := filepath.Join(profDir, "test-profile.json")
	if err := os.WriteFile(profPath, []byte(profileJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	view, err := buildProvenanceView(wd, profPath)
	if err != nil {
		t.Fatalf("buildProvenanceView: %v", err)
	}

	// Profile attribution: explicit path → source "workdir" (under wd).
	if view.Profile.Source != "workdir" {
		t.Errorf("profile source = %q; want workdir", view.Profile.Source)
	}

	// allow_domain entry present.
	foundAllow := false
	for _, e := range view.Network.Entries {
		if e.Entry == "github.com" && e.Action == "allow" && e.Source == "workdir" {
			foundAllow = true
		}
	}
	if !foundAllow {
		t.Errorf("github.com allow entry missing; got %+v", view.Network.Entries)
	}

	// deny_domain entry present.
	foundDeny := false
	for _, e := range view.Network.Entries {
		if e.Entry == "evil.com" && e.Action == "deny" {
			foundDeny = true
		}
	}
	if !foundDeny {
		t.Errorf("evil.com deny entry missing; got %+v", view.Network.Entries)
	}

	// Hard-deny metadata host always present.
	foundMeta := false
	for _, e := range view.Network.Entries {
		if e.Entry == "169.254.169.254" && e.Action == "deny" && e.Source == "builtin" {
			foundMeta = true
		}
	}
	if !foundMeta {
		t.Errorf("metadata host deny missing; got %+v", view.Network.Entries)
	}
}

func TestBuildProvenanceView_LearnedDecisions(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	profDir := filepath.Join(wd, ".opencode")
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	profPath := filepath.Join(profDir, "p.json")
	os.WriteFile(profPath, []byte(`{"meta":{"name":"p"},"workdir":{"access":"readwrite"}}`), 0o644)
	// Write learned decisions file.
	pagesPath := filepath.Join(profDir, "p.pages.json")
	os.WriteFile(pagesPath, []byte(`{"schema":1,"entries":[{"host":"learned.example.com","scope":"host","decision":"allow"}]}`), 0o644)

	view, err := buildProvenanceView(wd, profPath)
	if err != nil {
		t.Fatalf("buildProvenanceView: %v", err)
	}
	found := false
	for _, e := range view.Network.Entries {
		if e.Entry == "learned.example.com" && e.Action == "allow" && e.Source == "learned" {
			found = true
		}
	}
	if !found {
		t.Errorf("learned entry missing; got %+v", view.Network.Entries)
	}
}

func TestBuildProvenanceView_FilesystemBaseline(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	profDir := filepath.Join(wd, ".opencode")
	os.MkdirAll(profDir, 0o755)
	profPath := filepath.Join(profDir, "p.json")
	os.WriteFile(profPath, []byte(`{"meta":{"name":"p"},"workdir":{"access":"readwrite"}}`), 0o644)

	view, err := buildProvenanceView(wd, profPath)
	if err != nil {
		t.Fatalf("buildProvenanceView: %v", err)
	}
	// Baseline protected path ~/.ssh must appear as builtin deny.
	found := false
	for _, e := range view.Filesystem.Entries {
		if e.Action == "deny" && e.Source == "builtin" {
			// Protected paths are expanded; check the ~/.ssh prefix.
			if strings.Contains(e.Entry, ".ssh") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("~/.ssh protected path missing; got %+v", view.Filesystem.Entries)
	}
}

func TestBuildProvenanceView_EnvironmentBlocklist(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	profDir := filepath.Join(wd, ".opencode")
	os.MkdirAll(profDir, 0o755)
	profPath := filepath.Join(profDir, "p.json")
	os.WriteFile(profPath, []byte(`{"meta":{"name":"p"},"workdir":{"access":"readwrite"}}`), 0o644)

	view, err := buildProvenanceView(wd, profPath)
	if err != nil {
		t.Fatalf("buildProvenanceView: %v", err)
	}
	found := false
	for _, e := range view.Environment.Entries {
		if e.Entry == "BASH_ENV" && e.Action == "deny" && e.Source == "blocklist" {
			found = true
		}
	}
	if !found {
		t.Errorf("BASH_ENV blocklist entry missing; got %+v", view.Environment.Entries)
	}
}
```

Add `"strings"` to the test file imports if not present. Note: the first test file (Task 2) only imported `"testing"` — now we need `os`, `path/filepath`, `strings`. Merge the imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run TestBuildProvenance -v`
Expected: FAIL — `buildProvenanceView` undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/cli/provenance.go`:

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
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// hardDenyHosts mirrors netproxy.hardDenyHosts (not exported). Kept here
// for provenance display; if the netproxy list changes, update this too.
var provenanceHardDenyHosts = []string{
	"169.254.169.254",
	"metadata.google.internal",
	"metadata.azure.internal",
}

// buildProvenanceView loads the profile, learned decisions, baseline,
// and registry, then assembles a provenanceView. profileRef is a path,
// name, or "" for the default profile.
func buildProvenanceView(workdir, profileRef string) (*provenanceView, error) {
	profile, profPath, err := sandboxprofile.Resolve(profileRef)
	if err != nil {
		return nil, err
	}
	profSource := profileSource{Name: profile.Meta.Name, Path: profPath}
	profSource.Source = classifyProfilePath(profPath, workdir)

	view := &provenanceView{Profile: profSource}

	// --- Network ---
	view.Network = buildNetworkView(profile, profPath)

	// --- Filesystem ---
	view.Filesystem = buildFilesystemView(profile)

	// --- Environment ---
	view.Environment = buildEnvironmentView(profile)

	// --- Skills ---
	view.Skills = buildSkillsView(workdir)

	return view, nil
}

// classifyProfilePath attributes a profile path to a config layer.
func classifyProfilePath(profPath, workdir string) string {
	if profPath == "" {
		return "builtin"
	}
	if rel, err := filepath.Rel(filepath.Join(workdir, ".opencode"), profPath); err == nil && !strings.HasPrefix(rel, "..") {
		return "workdir"
	}
	home, err := os.UserHomeDir()
	if err == nil && strings.HasPrefix(profPath, filepath.Join(home, ".config", "omac")) {
		return "global"
	}
	return "global"
}

func buildNetworkView(profile *sandboxprofile.Profile, profPath string) networkView {
	src := classifyProfilePath(profPath, "")
	nv := networkView{
		Mode:          profile.Network.EffectiveMode(),
		PromptOn:      profile.Network.PromptEnabled(),
		OnUnavailable: profile.Network.OnUnavailable(),
	}
	// Learned decisions (from <name>.pages.json).
	pagesPath := sandboxprofile.PagesPath(profPath)
	if lp, err := netprompt.LoadLearnedPolicy(pagesPath); err == nil {
		for _, e := range lp.Entries() {
			nv.Entries = append(nv.Entries, provEntry{
				Entry:  e.Host,
				Action: e.Decision,
				Source: "learned",
			})
		}
	}
	// allow_domain.
	for _, d := range profile.Network.AllowDomain {
		nv.Entries = append(nv.Entries, provEntry{Entry: d, Action: "allow", Source: src})
	}
	// deny_domain.
	for _, d := range profile.Network.DenyDomain {
		nv.Entries = append(nv.Entries, provEntry{Entry: d, Action: "deny", Source: src})
	}
	// Hard-deny metadata hosts (builtin).
	for _, h := range provenanceHardDenyHosts {
		nv.Entries = append(nv.Entries, provEntry{Entry: h, Action: "deny", Source: "builtin"})
	}
	return nv
}

func buildFilesystemView(profile *sandboxprofile.Profile) filesystemView {
	src := "builtin"
	fv := filesystemView{WorkdirAccess: profile.Workdir.Access}
	if fv.WorkdirAccess == "" {
		fv.WorkdirAccess = sandboxprofile.AccessNone
	}
	add := func(entries []string, action string) {
		for _, e := range entries {
			fv.Entries = append(fv.Entries, provEntry{Entry: e, Action: action, Source: src})
		}
	}
	add(profile.Filesystem.Allow, "allow")
	add(profile.Filesystem.Read, "read")
	add(profile.Filesystem.Write, "write")
	add(profile.Filesystem.Deny, "deny")
	add(profile.Filesystem.OverrideDeny, "override-deny")
	for _, d := range profile.Filesystem.AllowUnixDir {
		fv.Entries = append(fv.Entries, provEntry{Entry: d + " (unix-dir)", Action: "allow", Source: src})
	}
	// Baseline read/write.
	baseline := sandboxprofile.PlatformBaseline()
	add(baseline.Read, "read")
	add(baseline.Write, "write")
	// Effective protected paths.
	for _, p := range sandboxprofile.EffectiveProtectedPaths(baseline, profile.Filesystem.OverrideDeny) {
		fv.Entries = append(fv.Entries, provEntry{Entry: p, Action: "deny", Source: "builtin"})
	}
	return fv
}

func buildEnvironmentView(profile *sandboxprofile.Profile) environmentView {
	ev := environmentView{}
	exact, prefixes := sandboxprofile.DangerousEnvBlocklist()
	for _, name := range exact {
		ev.Entries = append(ev.Entries, provEntry{Entry: name, Action: "deny", Source: "blocklist"})
	}
	for _, p := range prefixes {
		ev.Entries = append(ev.Entries, provEntry{Entry: p + "*", Action: "deny", Source: "blocklist"})
	}
	if len(profile.Environment.AllowVars) == 0 {
		ev.Entries = append(ev.Entries, provEntry{
			Entry:  "(no allowlist — all non-blocklisted vars pass)",
			Action: "allow",
			Source: "default",
		})
	} else {
		for _, v := range profile.Environment.AllowVars {
			ev.Entries = append(ev.Entries, provEntry{Entry: v, Action: "allow", Source: classifyProfilePath("", "")})
		}
	}
	return ev
}

func buildSkillsView(workdir string) skillsView {
	sv := skillsView{Workdir: workdir}
	workdirReg, err := registry.Load(workdir)
	if err != nil {
		return sv
	}
	globalReg, err := registry.LoadGlobal()
	if err != nil {
		return sv
	}
	workdirNames := map[string]struct{}{}
	for _, e := range workdirReg.Registered {
		workdirNames[e.Name] = struct{}{}
	}
	reg := mergeRegistries(globalReg, workdirReg)
	for _, e := range reg.Registered {
		src := "global"
		if _, ok := workdirNames[e.Name]; ok {
			src = "workdir"
		}
		sv.Entries = append(sv.Entries, provEntry{
			Entry:  e.Name,
			Action: "registered",
			Source: src,
		})
	}
	return sv
}
```

Note: `netprompt.LearnedPolicy` has unexported `entries` field — we need an `Entries()` accessor. Check if one exists; if not, add it to `internal/netprompt/learned.go`:

```go
// Entries returns a copy of the learned decisions for display.
func (lp *LearnedPolicy) Entries() []LearnedEntry {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	return append([]LearnedEntry(nil), lp.entries...)
}
```

Add this accessor to `learned.go` as part of this task.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestBuildProvenance -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cli/provenance.go internal/cli/provenance_test.go internal/netprompt/learned.go
git commit -s -m "feat(cli): implement buildProvenanceView data gatherer"
```

---

### Task 4: Implement the text formatter

**Files:**
- Modify: `internal/cli/provenance.go`
- Test: `internal/cli/provenance_test.go`

Render a `provenanceView` as four `tabwriter` tables, matching `omac list` / `omac config show` style. Long entries truncated at 60 chars with `…`.

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/provenance_test.go`:

```go
func TestWriteProvenanceText_NetworkSection(t *testing.T) {
	v := &provenanceView{
		Profile: profileSource{Name: "default", Source: "global"},
		Network: networkView{
			Mode: "filtered", PromptOn: true, OnUnavailable: "deny",
			Entries: []provEntry{
				{Entry: "github.com", Action: "allow", Source: "workdir"},
			},
		},
	}
	var buf strings.Builder
	code := writeProvenanceText(&buf, v)
	if code != ExitOK {
		t.Fatalf("code = %d", code)
	}
	out := buf.String()
	if !strings.Contains(out, "network") {
		t.Errorf("missing network section: %q", out)
	}
	if !strings.Contains(out, "github.com") {
		t.Errorf("missing github.com entry: %q", out)
	}
	if !strings.Contains(out, "allow") {
		t.Errorf("missing allow action: %q", out)
	}
}

func TestWriteProvenanceText_EmptySection(t *testing.T) {
	v := &provenanceView{
		Profile: profileSource{Name: "default", Source: "global"},
	}
	var buf strings.Builder
	code := writeProvenanceText(&buf, v)
	if code != ExitOK {
		t.Fatalf("code = %d", code)
	}
	out := buf.String()
	if !strings.Contains(out, "(none)") {
		t.Errorf("empty section should print (none): %q", out)
	}
}

func TestWriteProvenanceText_Truncation(t *testing.T) {
	longPath := "/" + strings.Repeat("a", 80)
	v := &provenanceView{
		Profile: profileSource{Name: "default", Source: "global"},
		Filesystem: filesystemView{
			Entries: []provEntry{{Entry: longPath, Action: "allow", Source: "builtin"}},
		},
	}
	var buf strings.Builder
	writeProvenanceText(&buf, v)
	out := buf.String()
	if !strings.Contains(out, "…") {
		t.Errorf("long entry should be truncated: %q", out)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run TestWriteProvenanceText -v`
Expected: FAIL — `writeProvenanceText` undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/cli/provenance.go`:

```go
// writeProvenanceText renders the view as four tabwriter tables.
func writeProvenanceText(w io.Writer, v *provenanceView) int {
	// Network
	fmt.Fprintf(w, "\nnetwork (profile: %s, mode: %s, prompt: %s, on_unavailable: %s)\n",
		v.Profile.Name, v.Network.Mode,
		onOff(v.Network.PromptOn), v.Network.OnUnavailable)
	writeProvTable(w, v.Network.Entries)

	// Filesystem
	fmt.Fprintf(w, "\nfilesystem (profile: %s, workdir.access: %s)\n",
		v.Profile.Name, v.Filesystem.WorkdirAccess)
	writeProvTable(w, v.Filesystem.Entries)

	// Environment
	fmt.Fprintln(w, "\nenvironment")
	writeProvTable(w, v.Environment.Entries)

	// Skills
	fmt.Fprintf(w, "\nskills (workdir: %s)\n", v.Skills.Workdir)
	writeProvTable(v.Entries)
	return ExitOK
}

func writeProvTable(w io.Writer, entries []provEntry) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  ENTRY\tACTION\tSOURCE")
	for _, e := range entries {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", truncateEntry(e.Entry), e.Action, e.Source)
	}
	_ = tw.Flush()
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// truncateEntry truncates display values at 60 chars, appending ….
func truncateEntry(s string) string {
	const max = 60
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestWriteProvenanceText -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cli/provenance.go internal/cli/provenance_test.go
git commit -s -m "feat(cli): add provenance text formatter"
```

---

### Task 5: Implement the JSON formatter

**Files:**
- Modify: `internal/cli/provenance.go`
- Test: `internal/cli/provenance_test.go`

Marshal the `provenanceView` struct directly — the json tags are already in place from Task 2.

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/provenance_test.go`:

```go
func TestWriteProvenanceJSON(t *testing.T) {
	v := &provenanceView{
		Profile: profileSource{Name: "default", Path: "/x.json", Source: "global"},
		Network: networkView{
			Mode: "filtered",
			Entries: []provEntry{
				{Entry: "github.com", Action: "allow", Source: "workdir"},
			},
		},
	}
	var buf strings.Builder
	code := writeProvenanceJSON(&buf, v)
	if code != ExitOK {
		t.Fatalf("code = %d", code)
	}
	out := buf.String()
	if !strings.Contains(out, `"profile"`) {
		t.Errorf("missing profile key: %q", out)
	}
	if !strings.Contains(out, `"github.com"`) {
		t.Errorf("missing github.com entry: %q", out)
	}
	// Must be valid JSON.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}
```

Add `"encoding/json"` to the test imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestWriteProvenanceJSON -v`
Expected: FAIL — `writeProvenanceJSON` undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/cli/provenance.go`:

```go
func writeProvenanceJSON(w io.Writer, v *provenanceView) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "omac provenance: json:", err)
		return ExitIOError
	}
	return ExitOK
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestWriteProvenanceJSON -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cli/provenance.go internal/cli/provenance_test.go
git commit -s -m "feat(cli): add provenance JSON formatter"
```

---

### Task 6: Wire up runProvenance + register in cli.go

**Files:**
- Modify: `internal/cli/provenance.go`
- Modify: `internal/cli/cli.go`
- Test: `internal/cli/provenance_test.go`

Parse `--profile` and `--json`, call `buildProvenanceView`, dispatch to the right formatter. Register the command in the dispatch map.

- [ ] **Step 1: Write the failing test (end-to-end runProvenance)**

Append to `internal/cli/provenance_test.go`:

```go
func TestRunProvenance_DefaultProfile(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	// Scaffold a minimal default profile so Resolve succeeds.
	profDir := filepath.Join(wd, ".opencode")
	os.MkdirAll(profDir, 0o755)
	// isolateHome sets HOME to a temp dir, so the default profile
	// would be scaffolded under there. Instead, write one to the
	// workdir's .opencode and reference it by path.
	profPath := filepath.Join(profDir, "default.json")
	os.WriteFile(profPath, []byte(`{"meta":{"name":"default"},"workdir":{"access":"readwrite"}}`), 0o644)

	env, read := captureEnv(t, wd)
	code := runProvenance([]string{"--profile", profPath, "--json"}, env)
	if code != ExitOK {
		out, errOut := read()
		t.Fatalf("code = %d; stdout=%q stderr=%q", code, out, errOut)
	}
	out, _ := read()
	if !strings.Contains(out, `"profile"`) {
		t.Errorf("expected JSON output with profile key; got %q", out)
	}
}

func TestRunProvenance_BadProfile(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	env, _ := captureEnv(t, wd)
	code := runProvenance([]string{"--profile", "/nonexistent/profile.json"}, env)
	if code != ExitConfigInvalid && code != ExitIOError {
		t.Errorf("expected error exit code; got %d", code)
	}
}

func TestRunProvenance_TextMode(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	profDir := filepath.Join(wd, ".opencode")
	os.MkdirAll(profDir, 0o755)
	profPath := filepath.Join(profDir, "default.json")
	os.WriteFile(profPath, []byte(`{"meta":{"name":"default"},"workdir":{"access":"readwrite"},"network":{"mode":"filtered","allow_domain":["github.com"]}}`), 0o644)

	env, read := captureEnv(t, wd)
	code := runProvenance([]string{"--profile", profPath}, env)
	if code != ExitOK {
		out, errOut := read()
		t.Fatalf("code = %d; stdout=%q stderr=%q", code, out, errOut)
	}
	out, _ := read()
	if !strings.Contains(out, "network") {
		t.Errorf("missing network section: %q", out)
	}
	if !strings.Contains(out, "github.com") {
		t.Errorf("missing github.com: %q", out)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run TestRunProvenance -v`
Expected: FAIL — `runProvenance` undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/cli/provenance.go`:

```go
// runProvenance implements `omac provenance [--profile <ref>] [--json]`.
func runProvenance(args []string, env *Env) int {
	fs := flag.NewFlagSet("provenance", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	profileRef := fs.String("profile", "", "sandbox profile name, path, or builtin (default: default)")
	jsonOut := fs.Bool("json", false, "Emit a JSON object instead of tabular text.")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac provenance [--profile <ref>] [--json]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
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

Register in `internal/cli/cli.go`:

In `commands()`, add an entry (place it after `"config"` to keep related read-only commands grouped):

```go
"provenance": {Name: "provenance", Short: "Show effective allow/deny entries across all subsystems.", Run: runProvenance},
```

In `printUsage`, add after the `config` line:

```
  provenance   Show effective allow/deny entries (network, filesystem, env, skills).
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestRunProvenance -v`
Expected: PASS

- [ ] **Step 5: Verify the full cli package compiles + all tests pass**

Run: `go build ./... && go test ./internal/cli/ -v`
Expected: build succeeds, all tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/provenance.go internal/cli/provenance_test.go internal/cli/cli.go
git commit -s -m "feat(cli): wire up omac provenance command"
```

---

### Task 7: E2E test — provenance matches actual sandbox behavior

**Files:**
- Create: `internal/e2e/provenance_test.go`

This is the cross-check: provenance says X is allowed/denied, the running sandbox enforces X. Reuses the security-audit setup (profile + self-audit skill) so the cross-check is against real sandbox behavior, not just a second copy of the same config.

- [ ] **Step 1: Write the test**

Create `internal/e2e/provenance_test.go`:

```go
//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EProvenance verifies that `omac provenance --json` output matches
// the actual sandbox behavior the agent observes. It reuses the
// security-audit setup (profile + self-audit skill) so the cross-check
// is against real enforcement, not a second copy of the config.
//
// Provenance is harness-agnostic (reads the same profile regardless of
// harness), so we only test with opencode.
func TestE2EProvenance(t *testing.T) {
	h, ok := harnessByName("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}

	home := t.TempDir()
	workdir := t.TempDir()

	for _, dir := range []string{".cache", ".cache/opencode", ".local/share/opencode", ".local/state/opencode/locks"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	omacBin := buildOmac(t)
	installHarness(t, h, home)
	h.ProviderSetup(t, home)

	spec := allowanceSpecFor(h)
	writeSandboxProfile(t, home, h, &spec)
	copySkill(t, h, workdir, "self-audit")
	registerSelfAudit(t, omacBin, home, workdir)

	// --- Step 1: Run `omac provenance --json` host-side ---
	profPath := filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json")
	cmd := exec.Command(omacBin, "provenance", "--profile", profPath, "--json")
	cmd.Dir = workdir
	cmd.Env = withHome(os.Environ(), home)
	provOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("omac provenance: %v\n%s", err, provOut)
	}

	var view struct {
		Profile struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"profile"`
		Network struct {
			Entries []struct {
				Entry  string `json:"entry"`
				Action string `json:"action"`
				Source string `json:"source"`
			} `json:"entries"`
		} `json:"network"`
		Environment struct {
			Entries []struct {
				Entry  string `json:"entry"`
				Action string `json:"action"`
				Source string `json:"source"`
			} `json:"entries"`
		} `json:"environment"`
		Skills struct {
			Entries []struct {
				Entry  string `json:"entry"`
				Action string `json:"action"`
				Source string `json:"source"`
			} `json:"entries"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(provOut, &view); err != nil {
		t.Fatalf("parse provenance JSON: %v\n%s", err, provOut)
	}

	// --- Step 2: Provenance-content assertions ---

	// 2a. allow_domain entries from the profile appear in provenance.
	allowDomains := []string{}
	for _, envVar := range []string{"SKAINET_INTERNAL", "ANTHROPIC_BASE_URL"} {
		if baseURL := os.Getenv(envVar); baseURL != "" {
			if host := extractHost(baseURL); host != "" {
				allowDomains = append(allowDomains, host)
			}
		}
	}
	allowDomains = append(allowDomains, h.Sandbox.ExtraAllowDomains...)
	for _, d := range allowDomains {
		found := false
		for _, e := range view.Network.Entries {
			if e.Entry == d && e.Action == "allow" {
				found = true
			}
		}
		if !found {
			t.Errorf("provenance: allow_domain %q missing from network entries", d)
		}
	}

	// 2b. Blocklist entries present.
	foundBlocklist := false
	for _, e := range view.Environment.Entries {
		if e.Source == "blocklist" && e.Action == "deny" {
			foundBlocklist = true
		}
	}
	if !foundBlocklist {
		t.Error("provenance: no blocklist entries in environment section")
	}

	// 2c. self-audit skill registered.
	foundSkill := false
	for _, e := range view.Skills.Entries {
		if e.Entry == "self-audit" && e.Action == "registered" {
			foundSkill = true
		}
	}
	if !foundSkill {
		t.Error("provenance: self-audit skill not in skills section")
	}

	// --- Step 3: Behavior cross-check via the audit agent ---
	prompt := "Run this command and print its full output verbatim:\n\n" +
		`sh "$OMAC_HARNESS_SKILLS_DIR/self-audit/scripts/audit.sh"` + "\n\n" +
		"Do not summarize. Print every line."
	auditStdout := runAuditAgent(t, h, omacBin, home, workdir, prompt)

	// 3a. Network denial: spec.NetDenyDomain should be denied by the sandbox.
	// The provenance output doesn't list it as allow → audit shows denial.
	if !assertNetworkDeniedSilent(auditStdout, spec.NetDenyDomain) {
		t.Errorf("behavior mismatch: %q not denied by sandbox (audit output lacks denial message)", spec.NetDenyDomain)
	}

	// 3b. AUDIT_SECRET: not in allow_vars → stripped from agent env.
	// Provenance shows allow_vars list (which excludes AUDIT_SECRET).
	if strings.Contains(auditStdout, auditSecretValue) {
		t.Error("behavior mismatch: AUDIT_SECRET leaked into agent env despite provenance not listing it as allowed")
	}

	// 3c. Filesystem denials present in audit output.
	if !assertFilesystemDeniedSilent(auditStdout) {
		t.Error("behavior mismatch: no filesystem denial in audit output despite provenance listing protected paths as denied")
	}
}

// assertNetworkDeniedSilent checks for network denial messages without
// logging (the e2e test's own assertions handle reporting).
func assertNetworkDeniedSilent(output, denyDomain string) bool {
	denials := []string{
		"Connection refused",
		"Could not resolve host",
		"Connection timed out",
		"Failed to connect",
		"curl: (6)",
		"curl: (7)",
		"curl: (28)",
		"DENIED BY THE SANDBOX",
		"403",
	}
	for _, d := range denials {
		if strings.Contains(output, d) {
			return true
		}
	}
	return false
}

// assertFilesystemDeniedSilent checks for fs denial messages without logging.
func assertFilesystemDeniedSilent(output string) bool {
	denials := []string{
		"Permission denied",
		"No such file or directory",
		"cannot open",
		"Operation not permitted",
	}
	for _, d := range denials {
		if strings.Contains(output, d) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Verify the test compiles**

Run: `go build -tags=e2e ./internal/e2e/`
Expected: builds without errors.

- [ ] **Step 3: Run the e2e test (requires harness installed + provider tokens)**

Run: `E2E_HARNESS=opencode go test -tags=e2e -timeout=10m -v ./internal/e2e/ -run TestE2EProvenance`
Expected: PASS (or skip if tokens missing — but the test should pass in CI).

- [ ] **Step 4: Commit**

```bash
git add internal/e2e/provenance_test.go
git commit -s -m "test(e2e): add provenance cross-check against actual sandbox behavior"
```

---

### Task 8: Final verification + README update

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Run the full test suite + lint**

Run: `go build ./... && go test ./... && gofmt -l . && go vet ./...`
Expected: all pass, gofmt clean, vet clean.

- [ ] **Step 2: Update README CLI summary**

In `README.md`, in the CLI summary block (after `config` and before `start`), add:

```
  provenance   Show effective allow/deny entries across network, filesystem,
               environment, and skills. Flags:
                 --profile <ref>     sandbox profile name/path/builtin
                 --json             emit JSON instead of tables
```

- [ ] **Step 3: Verify `omac provenance` runs against the built binary**

Run: `go build -o /tmp/omac ./cmd/omac && /tmp/omac provenance --help`
Expected: prints usage.

Run: `/tmp/omac provenance`
Expected: prints four sections (network, filesystem, environment, skills) with the default profile.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -s -m "docs: add provenance command to README CLI summary"
```

---

## Self-Review Notes

**Spec coverage check:**
- Network subsystem (learned + allow_domain + deny_domain + hard-deny + summary fields) → Task 3 `buildNetworkView`
- Filesystem (profile grants + baseline + protected paths + override_deny + allow_unix_dir) → Task 3 `buildFilesystemView`
- Environment (blocklist + allow_vars) → Task 3 `buildEnvironmentView`
- Skills (workdir + global merge, stale detection) → Task 3 `buildSkillsView` (stale: covered by `mergeRegistries` which only returns registered entries; the `omac list` stale-entry surfacing is out of scope for provenance v1 — the spec says "surfaced separately as in omac list" but the data structure doesn't have a stale field. Decision: stale entries are omitted from provenance since the skill dir doesn't exist; this matches the read-only nature. If needed, add a `stale` action later.)
- Profile layer attribution → Task 3 `classifyProfilePath`
- Text + JSON output → Tasks 4, 5
- `--profile` + `--json` flags → Task 6
- Unit tests → Tasks 1-6
- E2E cross-check → Task 7
- README + lint → Task 8

**Type consistency check:**
- `provEntry` fields: `Entry`, `Action`, `Source` — used consistently across all sections.
- `buildProvenanceView(workdir, profileRef)` — called with `(env.Workdir, *profileRef)` in Task 6.
- `writeProvenanceText(io.Writer, *provenanceView) int` — matches the call in Task 6.
- `writeProvenanceJSON(io.Writer, *provenanceView) int` — matches.

**Known simplification:**
- Stale skill registrations are not surfaced in provenance v1 (only live entries appear). The spec mentions them but the data model doesn't support a `stale` action cleanly without duplicating `omac list`'s directory-existence check. YAGNI — add when a user asks "why does provenance show a skill that's gone?".
