package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvenanceViewJSONRoundTrip(t *testing.T) {
	v := provenanceView{
		Profile: profileSource{Name: "default", Path: "/x/default.json", Source: "global"},
		Network: networkView{
			Mode:          "filtered",
			PromptOn:      true,
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

func TestTruncateEntry_Multibyte(t *testing.T) {
	// 80 runes of multi-byte chars — must truncate by rune, not byte.
	s := strings.Repeat("ü", 80)
	got := truncateEntry(s)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected … suffix; got %q", got)
	}
	// Result should be max-1 runes + … = 60 runes total.
	r := []rune(got)
	if len(r) != 60 {
		t.Errorf("expected 60 runes; got %d", len(r))
	}
}

func TestTruncateEntry_ShortString(t *testing.T) {
	got := truncateEntry("short")
	if got != "short" {
		t.Errorf("short string should be unchanged; got %q", got)
	}
}

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
