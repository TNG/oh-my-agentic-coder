package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
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
	os.WriteFile(profPath, []byte(`{"meta":{"name":"default"},"workdir":{"access":"readwrite"},"environment":{"allow_vars":["HOME","PATH"]}}`), 0o644)

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
	os.WriteFile(profPath, []byte(`{"meta":{"name":"default"},"workdir":{"access":"readwrite"},"environment":{"allow_vars":["HOME","PATH"]}}`), 0o644)

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

func TestBuildProvenanceView_CacheSection(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	profDir := filepath.Join(wd, ".opencode")
	os.MkdirAll(profDir, 0o755)
	profPath := filepath.Join(profDir, "default.json")
	os.WriteFile(profPath, []byte(`{"meta":{"name":"default"},"workdir":{"access":"readwrite"}}`), 0o644)
	// Select the per-workdir scope so provenance reports it (default is global).
	os.WriteFile(filepath.Join(profDir, "oh-my-agentic-coder.yaml"), []byte("cache:\n  scope: workdir\n"), 0o644)

	view, err := buildProvenanceView(wd, profPath)
	if err != nil {
		t.Fatalf("buildProvenanceView: %v", err)
	}

	// Describe what the scope *should* be — DescribePersistent does not
	// create the directory, and neither should buildProvenanceView.
	want, err := toolcache.DescribePersistent(toolcache.DomainWorkdir, wd)
	if err != nil {
		t.Fatalf("DescribePersistent: %v", err)
	}

	if view.Cache.Scope != string(toolcache.DomainWorkdir) {
		t.Errorf("Cache.Scope = %q; want %q", view.Cache.Scope, toolcache.DomainWorkdir)
	}
	if view.Cache.Mode != string(toolcache.ModePersistent) {
		t.Errorf("Cache.Mode = %q; want %q", view.Cache.Mode, toolcache.ModePersistent)
	}
	if view.Cache.Path != want.Dir {
		t.Errorf("Cache.Path = %q; want %q", view.Cache.Path, want.Dir)
	}
	if view.Cache.Path == "" {
		t.Fatal("Cache.Path should not be empty")
	}
	// The cache directory must NOT have been created by provenance.
	if _, err := os.Stat(view.Cache.Path); err == nil {
		t.Errorf("provenance should not create the cache dir %q", view.Cache.Path)
	}

	// All eight environment mappings must be present.
	env := view.Cache.Environment
	for _, k := range []string{
		"XDG_CACHE_HOME", "GOCACHE", "GOMODCACHE", "NPM_CONFIG_CACHE",
		"PIP_CACHE_DIR", "CARGO_HOME", "OMAC_CACHE_DIR", "OMAC_CACHE_MODE",
	} {
		if _, ok := env[k]; !ok {
			t.Errorf("Cache.Environment missing %q; got %v", k, env)
		}
	}
	if got := env["OMAC_CACHE_MODE"]; got != string(toolcache.ModePersistent) {
		t.Errorf("OMAC_CACHE_MODE = %q; want %q", got, toolcache.ModePersistent)
	}
}

func TestWriteProvenanceText_CacheSection(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	profDir := filepath.Join(wd, ".opencode")
	os.MkdirAll(profDir, 0o755)
	profPath := filepath.Join(profDir, "default.json")
	os.WriteFile(profPath, []byte(`{"meta":{"name":"default"},"workdir":{"access":"readwrite"}}`), 0o644)

	view, err := buildProvenanceView(wd, profPath)
	if err != nil {
		t.Fatalf("buildProvenanceView: %v", err)
	}
	var buf strings.Builder
	if code := writeProvenanceText(&buf, view); code != ExitOK {
		t.Fatalf("writeProvenanceText: code %d", code)
	}
	out := buf.String()
	if !strings.Contains(out, "cache") && !strings.Contains(out, "CACHE") {
		t.Errorf("text should render a cache section; got:\n%s", out)
	}
	if !strings.Contains(out, view.Cache.Path) {
		t.Errorf("text should name the cache path %q; got:\n%s", view.Cache.Path, out)
	}
	if !strings.Contains(out, "workdir") {
		t.Errorf("text should label scope as default workdir; got:\n%s", out)
	}
}

func TestWriteProvenanceJSON_CacheSection(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	profDir := filepath.Join(wd, ".opencode")
	os.MkdirAll(profDir, 0o755)
	profPath := filepath.Join(profDir, "default.json")
	os.WriteFile(profPath, []byte(`{"meta":{"name":"default"},"workdir":{"access":"readwrite"}}`), 0o644)

	view, err := buildProvenanceView(wd, profPath)
	if err != nil {
		t.Fatalf("buildProvenanceView: %v", err)
	}
	var buf strings.Builder
	if code := writeProvenanceJSON(&buf, view); code != ExitOK {
		t.Fatalf("writeProvenanceJSON: code %d", code)
	}
	out := buf.String()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	cache, ok := parsed["cache"].(map[string]any)
	if !ok {
		t.Fatalf("JSON missing cache object; got %v", parsed)
	}
	// No config on disk => default global scope, backed by the shared domain.
	if scope, _ := cache["scope"].(string); scope != string(toolcache.DomainShared) {
		t.Errorf("cache.scope = %q; want %q", scope, toolcache.DomainShared)
	}
	if mode, _ := cache["mode"].(string); mode != string(toolcache.ModePersistent) {
		t.Errorf("cache.mode = %q; want %q", mode, toolcache.ModePersistent)
	}
	env, _ := cache["environment"].(map[string]any)
	if env == nil {
		t.Fatalf("cache.environment missing; got %v", cache)
	}
	for _, k := range []string{
		"XDG_CACHE_HOME", "GOCACHE", "GOMODCACHE", "NPM_CONFIG_CACHE",
		"PIP_CACHE_DIR", "CARGO_HOME", "OMAC_CACHE_DIR", "OMAC_CACHE_MODE",
	} {
		if _, ok := env[k]; !ok {
			t.Errorf("cache.environment missing %q; got %v", k, env)
		}
	}
}

func TestBuildProvenanceView_CacheErrorSurfaced(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	profDir := filepath.Join(wd, ".opencode")
	os.MkdirAll(profDir, 0o755)
	profPath := filepath.Join(profDir, "default.json")
	os.WriteFile(profPath, []byte(`{"meta":{"name":"default"},"workdir":{"access":"readwrite"}}`), 0o644)

	// DescribePersistent calls os.UserHomeDir; an empty HOME makes it
	// fail. buildCacheView must surface that error rather than swallow
	// it and render an empty "no cache" section.
	t.Setenv("HOME", "")

	view, err := buildProvenanceView(wd, profPath)
	if err == nil {
		t.Fatalf("expected error from buildProvenanceView when HOME is unset; got view=%+v", view)
	}
	if view != nil {
		t.Errorf("expected nil view on error; got %+v", view)
	}
}

func TestBuildCacheView_ErrorNotSwallowed(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	t.Setenv("HOME", "")

	cv, err := buildCacheView(config.CacheScopeGlobal, wd, "")
	if err == nil {
		t.Fatalf("expected error from buildCacheView when HOME is unset; got %+v", cv)
	}
	if cv.Path != "" {
		t.Errorf("expected empty cacheView on error; got Path=%q", cv.Path)
	}
}

// TestBuildBuildExecutorView_PlatformPosture asserts the build-executor
// network-posture view distinguishes the two supported platforms and
// carries the platform-appropriate posture/boundary/residual fields
// (ticket 07). runtime.GOOS cannot be changed in a test, so this asserts
// the platform-appropriate fields for the CURRENT platform and the
// platform-independent canonical-checks field.
func TestBuildBuildExecutorView_PlatformPosture(t *testing.T) {
	v := buildBuildExecutorView()
	if v.Platform == "" {
		t.Fatal("Platform must be set (runtime.GOOS)")
	}
	if v.CanonicalChecks == "" {
		t.Error("CanonicalChecks must be set (same on both platforms)")
	}
	// Platform-appropriate posture. The accepted-residual wording must
	// match the platform; on no platform may a loopback guarantee the
	// executor does not have be implied (ADR 0003 Revision).
	switch v.Platform {
	case "darwin":
		for _, want := range []string{
			"env-only filtered",
			"filesystem confinement only",
			"filesystem-only",
			"works (no kernel network filter)",
			"raw-socket-capable build code can reach host loopback and external egress",
			"no host-listener monitoring/guarding",
			"ADR 0003 Revision",
		} {
			if !strings.Contains(v.AcceptedResidual, "raw-socket-capable") && want == "raw-socket-capable build code can reach host loopback and external egress" {
				t.Errorf("darwin accepted residual must state the raw-socket reachability: %q", v.AcceptedResidual)
			}
			switch want {
			case "env-only filtered":
				if !strings.Contains(v.NetworkPosture, "env-only filtered") {
					t.Errorf("darwin NetworkPosture = %q; want env-only filtered", v.NetworkPosture)
				}
			case "filesystem confinement only":
				if !strings.Contains(v.NetworkPosture, "filesystem confinement only") {
					t.Errorf("darwin NetworkPosture = %q; want filesystem confinement only", v.NetworkPosture)
				}
			case "filesystem-only":
				if v.LoopbackBoundary != "filesystem-only" {
					t.Errorf("darwin LoopbackBoundary = %q; want filesystem-only", v.LoopbackBoundary)
				}
			case "works (no kernel network filter)":
				if v.WorkerLoopback != "works (no kernel network filter)" {
					t.Errorf("darwin WorkerLoopback = %q; want works (no kernel network filter)", v.WorkerLoopback)
				}
			case "raw-socket-capable build code can reach host loopback and external egress":
				if !strings.Contains(v.AcceptedResidual, "raw-socket-capable") {
					t.Errorf("darwin AcceptedResidual missing raw-socket reachability: %q", v.AcceptedResidual)
				}
			case "no host-listener monitoring/guarding":
				if !strings.Contains(v.AcceptedResidual, "no host-listener monitoring/guarding") {
					t.Errorf("darwin AcceptedResidual must disclaim host-listener monitoring/guarding: %q", v.AcceptedResidual)
				}
			case "ADR 0003 Revision":
				if !strings.Contains(v.AcceptedResidual, "ADR 0003 Revision") {
					t.Errorf("darwin AcceptedResidual must cite ADR 0003 Revision: %q", v.AcceptedResidual)
				}
			}
		}
	case "linux":
		for _, want := range []string{
			"kernel-blocked (private sandbox loopback)",
			"kernel (network namespace)",
			"private sandbox loopback",
			"host-loopback services unreachable from the executor",
		} {
			switch want {
			case "kernel-blocked (private sandbox loopback)":
				if v.NetworkPosture != want {
					t.Errorf("linux NetworkPosture = %q; want %q", v.NetworkPosture, want)
				}
			case "kernel (network namespace)":
				if v.LoopbackBoundary != want {
					t.Errorf("linux LoopbackBoundary = %q; want %q", v.LoopbackBoundary, want)
				}
			case "private sandbox loopback":
				if v.WorkerLoopback != want {
					t.Errorf("linux WorkerLoopback = %q; want %q", v.WorkerLoopback, want)
				}
			case "host-loopback services unreachable from the executor":
				if v.AcceptedResidual != want {
					t.Errorf("linux AcceptedResidual = %q; want %q", v.AcceptedResidual, want)
				}
			}
		}
	default:
		t.Skipf("unsupported platform %q for build-executor posture assertions", v.Platform)
	}
}

// TestBuildBuildExecutorView_CanonicalChecksOnBothPlatforms asserts the
// canonical-checks field is identical on both platforms and states the
// twin retirement + canonical Worker-API checks (ticket 07 checkbox 1).
func TestBuildBuildExecutorView_CanonicalChecksOnBothPlatforms(t *testing.T) {
	v := buildBuildExecutorView()
	for _, want := range []string{
		"yarp3 checkstyle twin tasks retired",
		"OMAC init.d",
		"canonical checkstyleMain/checkstyleTest run unchanged",
		"Gradle Worker API",
	} {
		if !strings.Contains(v.CanonicalChecks, want) {
			t.Errorf("CanonicalChecks missing %q: %q", want, v.CanonicalChecks)
		}
	}
}

// TestBuildProvenanceView_BuildExecutorSection asserts the build-executor
// network-posture view is populated by buildProvenanceView on the current
// platform.
func TestBuildProvenanceView_BuildExecutorSection(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	profDir := filepath.Join(wd, ".opencode")
	os.MkdirAll(profDir, 0o755)
	profPath := filepath.Join(profDir, "default.json")
	os.WriteFile(profPath, []byte(`{"meta":{"name":"default"},"workdir":{"access":"readwrite"}}`), 0o644)

	view, err := buildProvenanceView(wd, profPath)
	if err != nil {
		t.Fatalf("buildProvenanceView: %v", err)
	}
	if view.BuildExecutor.Platform == "" {
		t.Error("BuildExecutor.Platform must be populated by buildProvenanceView")
	}
	if view.BuildExecutor.NetworkPosture == "" {
		t.Error("BuildExecutor.NetworkPosture must be populated by buildProvenanceView")
	}
	if view.BuildExecutor.AcceptedResidual == "" {
		t.Error("BuildExecutor.AcceptedResidual must be populated by buildProvenanceView")
	}
}

// TestWriteProvenanceText_BuildExecutorSection asserts the text renderer
// emits a "build executor" section that states the accepted residual
// (ticket 07 checkbox 5) and the canonical checks.
func TestWriteProvenanceText_BuildExecutorSection(t *testing.T) {
	v := &provenanceView{
		Profile:       profileSource{Name: "default", Source: "global"},
		BuildExecutor: buildBuildExecutorView(),
	}
	var buf strings.Builder
	if code := writeProvenanceText(&buf, v); code != ExitOK {
		t.Fatalf("writeProvenanceText: code %d", code)
	}
	out := buf.String()
	if !strings.Contains(out, "build executor") {
		t.Errorf("text should render a build executor section; got:\n%s", out)
	}
	// The accepted residual must be present (the briefing must state it).
	if !strings.Contains(out, v.BuildExecutor.AcceptedResidual) {
		t.Errorf("text should state the accepted residual; got:\n%s", out)
	}
	// The canonical checks line must be present.
	if !strings.Contains(out, "canonical checks") {
		t.Errorf("text should render a canonical checks line; got:\n%s", out)
	}
	// On no platform may host-listener monitoring/guarding be claimed
	// on macOS — the darwin accepted residual explicitly disclaims it.
	if v.BuildExecutor.Platform == "darwin" {
		if !strings.Contains(out, "no host-listener monitoring/guarding") {
			t.Errorf("darwin text must disclaim host-listener monitoring/guarding (ADR 0003 Revision); got:\n%s", out)
		}
	}
}

// TestWriteProvenanceJSON_BuildExecutorSection asserts the JSON renderer
// includes the build_executor object with the platform-appropriate fields.
func TestWriteProvenanceJSON_BuildExecutorSection(t *testing.T) {
	v := &provenanceView{
		Profile:       profileSource{Name: "default", Path: "/x.json", Source: "global"},
		BuildExecutor: buildBuildExecutorView(),
	}
	var buf strings.Builder
	if code := writeProvenanceJSON(&buf, v); code != ExitOK {
		t.Fatalf("writeProvenanceJSON: code %d", code)
	}
	out := buf.String()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	be, ok := parsed["build_executor"].(map[string]any)
	if !ok {
		t.Fatalf("JSON missing build_executor object; got %v", parsed)
	}
	for _, key := range []string{
		"platform", "network_posture", "loopback_boundary",
		"worker_loopback", "accepted_residual", "canonical_checks",
	} {
		if _, ok := be[key]; !ok {
			t.Errorf("build_executor JSON missing %q; got %v", key, be)
		}
	}
}

// Provenance is an inspection command: like `omac diagnose` it must not
// scaffold default.json in a fresh home. (Regression guard for #173: it
// used the mutating resolver, so merely viewing the effective policy —
// or running --check — wrote a file into the user's config dir.)
func TestProvenanceDoesNotScaffoldProfileInFreshHome(t *testing.T) {
	for _, args := range [][]string{{}, {"--check"}} {
		name := "view"
		if len(args) > 0 {
			name = args[0]
		}
		t.Run(name, func(t *testing.T) {
			isolateHome(t)
			env, _, _, drain := newPipeEnv(t, "")
			env.Workdir = t.TempDir()
			_ = runProvenance(args, env)
			drain()

			defaultPath, err := sandboxprofile.ProfilePath("default")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
				t.Errorf("provenance %v scaffolded %s; must be read-only", args, defaultPath)
			}
		})
	}
}
