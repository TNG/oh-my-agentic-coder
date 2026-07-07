package sandboxprofile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFlagsAllowUnixDir(t *testing.T) {
	for _, argv := range [][]string{
		{"--allow-unix-dir", "/tmp/cc-daemon-502", "--", "true"},
		{"--allow-unix-dir=/tmp/cc-daemon-502", "--", "true"},
	} {
		f, err := ParseFlags(argv)
		if err != nil {
			t.Fatalf("ParseFlags(%v): %v", argv, err)
		}
		if len(f.AllowUnixDir) != 1 || f.AllowUnixDir[0] != "/tmp/cc-daemon-502" {
			t.Fatalf("AllowUnixDir not parsed from %v: %v", argv, f.AllowUnixDir)
		}
		p, _ := Merge(&Profile{}, f)
		found := false
		for _, d := range p.Filesystem.AllowUnixDir {
			if d == "/tmp/cc-daemon-502" {
				found = true
			}
		}
		if !found {
			t.Errorf("Merge dropped AllowUnixDir: %v", p.Filesystem.AllowUnixDir)
		}
	}
}

func TestParseValidProfile(t *testing.T) {
	data := []byte(`{
	  "meta": {"name": "tng-sandbox"},
	  "workdir": {"access": "readwrite"},
	  "filesystem": {
	    "allow": [".", "~/.cache"],
	    "read": ["~/.gitconfig"],
	    "override_deny": ["~/.git-credentials"]
	  },
	  "network": {
	    "listen_port": [4097],
	    "allow_tcp_connect": [22],
	    "network_prompt": {"enabled": true, "prompt_timeout_secs": 60, "on_unavailable": "deny"}
	  }
	}`)
	p, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Workdir.Access != AccessReadWrite {
		t.Errorf("workdir.access = %q", p.Workdir.Access)
	}
	if !p.Network.PromptEnabled() {
		t.Error("prompt should be enabled")
	}
	if p.Network.PromptTimeoutSecs() != 60 {
		t.Errorf("timeout = %d", p.Network.PromptTimeoutSecs())
	}
	if p.Network.OnUnavailable() != OnUnavailableDeny {
		t.Errorf("on_unavailable = %q", p.Network.OnUnavailable())
	}
	if p.Network.EffectiveMode() != ModeFiltered {
		t.Errorf("mode = %q", p.Network.EffectiveMode())
	}
	if got := p.Filesystem.OverrideDeny; len(got) != 1 || got[0] != "~/.git-credentials" {
		t.Errorf("override_deny = %v", got)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	cases := []string{
		`{"security": {"groups": []}}`,
		`{"filesystem": {"allow_file": []}}`,
		`{"network": {"credentials": ["tng_skills"]}}`,
		`{"network": {"network_prompt": {"learned_policy_path": "/x"}}}`,
	}
	for _, c := range cases {
		if _, err := Parse([]byte(c)); err == nil {
			t.Errorf("Parse(%s) should fail on unknown field", c)
		}
	}
}

func TestParseValidationErrors(t *testing.T) {
	cases := []string{
		`{"workdir": {"access": "rw"}}`,
		`{"network": {"mode": "openish"}}`,
		`{"network": {"enforcement": "none"}}`,
		`{"network": {"listen_port": [70000]}}`,
		`{"network": {"network_prompt": {"on_unavailable": "ask"}}}`,
		`{"environment": {"allow_vars": [" "]}}`,
		`{"filesystem": {"deny": [" "]}}`,   // empty deny entry
		`{"filesystem": {"deny": ["[a-"]}}`, // malformed basename glob
	}
	for _, c := range cases {
		if _, err := Parse([]byte(c)); err == nil {
			t.Errorf("Parse(%s) should fail validation", c)
		}
	}
}

func TestParseDeny(t *testing.T) {
	p, err := Parse([]byte(`{"filesystem": {"deny": [".env", "*.key", "~/.aws", "./config/prod.env"]}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{".env", "*.key", "~/.aws", "./config/prod.env"}
	if len(p.Filesystem.Deny) != len(want) {
		t.Fatalf("deny = %v, want %v", p.Filesystem.Deny, want)
	}
	for i, w := range want {
		if p.Filesystem.Deny[i] != w {
			t.Errorf("deny[%d] = %q, want %q", i, p.Filesystem.Deny[i], w)
		}
	}
}

func TestIsBasenameGlob(t *testing.T) {
	glob := []string{".env", "*.key", "secret", "prod*.env"}
	pathy := []string{"~/.env", "$HOME/.env", "/etc/passwd", "./x", "a/b", "config/prod.env"}
	for _, g := range glob {
		if !IsBasenameGlob(g) {
			t.Errorf("%q should be a basename glob", g)
		}
	}
	for _, p := range pathy {
		if IsBasenameGlob(p) {
			t.Errorf("%q should be treated as a path", p)
		}
	}
}

func TestPromptDefaultsWhenObjectPresent(t *testing.T) {
	p, err := Parse([]byte(`{"network": {"network_prompt": {}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Network.PromptEnabled() {
		t.Error("prompt object present means enabled (nono semantics)")
	}
	if p.Network.PromptTimeoutSecs() != DefaultPromptTimeoutSecs {
		t.Errorf("timeout = %d", p.Network.PromptTimeoutSecs())
	}
	p2, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if p2.Network.PromptEnabled() {
		t.Error("absent prompt object means disabled")
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	got, err := ExpandPath("~/.gitconfig")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(home, ".gitconfig") {
		t.Errorf("got %q", got)
	}
	t.Setenv("OMAC_TEST_DIR", "/tmp/omac-test")
	got, err = ExpandPath("$OMAC_TEST_DIR/sub")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/omac-test/sub" {
		t.Errorf("got %q", got)
	}
	if _, err := ExpandPath(""); err == nil {
		t.Error("empty path should fail")
	}
}

func TestExpandExistingSkipsMissing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope")
	var buf strings.Builder
	out, err := ExpandExisting([]string{dir, missing}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0] != dir {
		t.Errorf("out = %v", out)
	}
	if !strings.Contains(buf.String(), "skipping nonexistent path") {
		t.Errorf("notice missing: %q", buf.String())
	}
}

func TestResolveFirstStartScaffoldsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, path, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json")
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	// File must now exist, pretty-printed, and parse back to the same settings.
	raw, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("default.json not scaffolded: %v", err)
	}
	if !strings.Contains(string(raw), "\n  ") || !strings.HasSuffix(string(raw), "\n") {
		t.Error("scaffolded default.json is not pretty-printed")
	}
	if p.Workdir.Access != AccessReadWrite {
		t.Errorf("default workdir.access = %q", p.Workdir.Access)
	}
	if len(p.Network.ListenPort) != 1 || p.Network.ListenPort[0] != 4097 {
		t.Errorf("default listen_port = %v", p.Network.ListenPort)
	}
	if len(p.Network.AllowTCPConnect) != 1 || p.Network.AllowTCPConnect[0] != 22 {
		t.Errorf("default allow_tcp_connect = %v", p.Network.AllowTCPConnect)
	}
	if !p.Network.PromptEnabled() {
		t.Error("default prompt should be enabled")
	}
}

func TestDefaultProfileGrantsSharedAgentsSkillsRead(t *testing.T) {
	// The shared neutral skills base ("agents") is in scope for every
	// harness, so its user-global roots must be readable inside the
	// sandbox. Per-harness global skills dirs are granted RW via
	// Harness.SandboxDirs; only the shared base needs an explicit read
	// grant in the default profile.
	p := DefaultProfile()
	want := []string{"~/.config/agents/skills", "~/.agents/skills"}
	for _, w := range want {
		found := false
		for _, r := range p.Filesystem.Read {
			if r == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DefaultProfile.Filesystem.Read missing %q (got %v)", w, p.Filesystem.Read)
		}
	}
}

func TestResolveExistingDefaultWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "omac", "sandbox-profiles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "default.json"),
		[]byte(`{"workdir": {"access": "read"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, _, err := Resolve("default")
	if err != nil {
		t.Fatal(err)
	}
	if p.Workdir.Access != AccessRead {
		t.Errorf("edited file should win, got access %q", p.Workdir.Access)
	}
	if len(p.Network.ListenPort) != 0 {
		t.Error("compiled-in defaults must not be merged into the user file")
	}
}

func TestResolveExplicitPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.json")
	if err := os.WriteFile(path, []byte(`{"meta": {"name": "custom"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, gotPath, err := Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Meta.Name != "custom" {
		t.Errorf("name = %q", p.Meta.Name)
	}
	if gotPath != path {
		t.Errorf("returned path = %q", gotPath)
	}
}

func TestResolveUnknownProfileNamesExpectedPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := Resolve("nosuch")
	if err == nil {
		t.Fatal("expected error (only default is auto-created)")
	}
	if !strings.Contains(err.Error(), "sandbox-profiles/nosuch.json") {
		t.Errorf("error should name the expected path: %v", err)
	}
}

func TestPagesPath(t *testing.T) {
	if got := PagesPath("/x/sandbox-profiles/default.json"); got != "/x/sandbox-profiles/default.pages.json" {
		t.Errorf("PagesPath = %q", got)
	}
}

func TestParseFlagsLauncherStyle(t *testing.T) {
	f, err := ParseFlags([]string{
		"--profile", "tng-sandbox",
		"--allow-file", "/tmp/x/bridge.sock",
		"--read", "/tmp/x",
		"--read", "/tmp/t", "--write", "/tmp/t",
		"--open-port", "49152",
		"--", "opencode", "--port", "4097",
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.ProfileRef != "tng-sandbox" {
		t.Errorf("profile = %q", f.ProfileRef)
	}
	if len(f.AllowFile) != 1 || f.AllowFile[0] != "/tmp/x/bridge.sock" {
		t.Errorf("allow-file = %v", f.AllowFile)
	}
	if len(f.Read) != 2 || len(f.Write) != 1 {
		t.Errorf("read=%v write=%v", f.Read, f.Write)
	}
	if len(f.OpenPort) != 1 || f.OpenPort[0] != 49152 {
		t.Errorf("open-port = %v", f.OpenPort)
	}
	if len(f.InnerArgv) != 3 || f.InnerArgv[0] != "opencode" {
		t.Errorf("inner = %v", f.InnerArgv)
	}
}

func TestParseFlagsErrors(t *testing.T) {
	cases := [][]string{
		{"--open-port", "0", "--", "x"},
		{"--open-port", "abc", "--", "x"},
		{"--workdir-access", "rw", "--", "x"},
		{"--unknown", "--", "x"},
		{"--read", "/x"},       // no -- separator
		{"--read", "/x", "--"}, // no command after --
		{"--read"},             // missing value
	}
	for _, c := range cases {
		if _, err := ParseFlags(c); err == nil {
			t.Errorf("ParseFlags(%v) should fail", c)
		}
	}
}

func TestParseFlagsInlineValues(t *testing.T) {
	f, err := ParseFlags([]string{"--profile=p", "--open-port=8080", "--", "true"})
	if err != nil {
		t.Fatal(err)
	}
	if f.ProfileRef != "p" || len(f.OpenPort) != 1 || f.OpenPort[0] != 8080 {
		t.Errorf("inline parse failed: %+v", f)
	}
}

func TestParseAndMergeDenyFlag(t *testing.T) {
	f, err := ParseFlags([]string{"--deny", ".env", "--deny=*.key", "--", "true"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Deny) != 2 || f.Deny[0] != ".env" || f.Deny[1] != "*.key" {
		t.Fatalf("deny flags = %v", f.Deny)
	}
	p := &Profile{Filesystem: Filesystem{Deny: []string{"~/.aws"}}}
	merged, _ := Merge(p, f)
	if len(merged.Filesystem.Deny) != 3 {
		t.Errorf("merged deny = %v, want 3 (profile + 2 flags)", merged.Filesystem.Deny)
	}
	if len(p.Filesystem.Deny) != 1 {
		t.Error("Merge mutated base profile deny")
	}
}

func TestMergeAdditive(t *testing.T) {
	p := &Profile{
		Filesystem: Filesystem{Read: []string{"~/.gitconfig"}},
		Network:    Network{OpenPort: []int{1000}, AllowDomain: []string{"github.com"}},
		Workdir:    Workdir{Access: AccessRead},
	}
	f := &Flags{
		Read:          []string{"/tmp/x"},
		AllowFile:     []string{"/tmp/x/s.sock"},
		OpenPort:      []int{49152},
		AllowDomain:   []string{"npmjs.org"},
		WorkdirAccess: AccessReadWrite,
	}
	merged, warnings := Merge(p, f)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v", warnings)
	}
	if len(merged.Filesystem.Read) != 2 {
		t.Errorf("read = %v", merged.Filesystem.Read)
	}
	if len(merged.Filesystem.Allow) != 1 {
		t.Errorf("allow = %v (allow-file should merge into allow)", merged.Filesystem.Allow)
	}
	if len(merged.Network.OpenPort) != 2 {
		t.Errorf("open_port = %v", merged.Network.OpenPort)
	}
	if len(merged.Network.AllowDomain) != 2 {
		t.Errorf("allow_domain = %v", merged.Network.AllowDomain)
	}
	if merged.Workdir.Access != AccessReadWrite {
		t.Errorf("workdir = %q", merged.Workdir.Access)
	}
	// base profile untouched
	if len(p.Filesystem.Read) != 1 || p.Workdir.Access != AccessRead {
		t.Error("Merge mutated the base profile")
	}
}

func TestMergeBlockNetOverrides(t *testing.T) {
	p := &Profile{
		Network: Network{
			AllowDomain:   []string{"github.com"},
			NetworkPrompt: &NetworkPrompt{},
		},
	}
	merged, warnings := Merge(p, &Flags{BlockNet: true})
	if merged.Network.EffectiveMode() != ModeBlocked {
		t.Errorf("mode = %q", merged.Network.EffectiveMode())
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "--block-net overrides") {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestValidateOpenPortZeroSentinel(t *testing.T) {
	p := &Profile{Network: Network{OpenPort: []int{0, 4097}}}
	if err := p.Validate(); err != nil {
		t.Errorf("open_port 0 sentinel rejected: %v", err)
	}
	// The sentinel is open_port-only; 0 stays invalid for the others.
	if err := (&Profile{Network: Network{ListenPort: []int{0}}}).Validate(); err == nil {
		t.Error("listen_port 0 must be rejected")
	}
	if err := (&Profile{Network: Network{AllowTCPConnect: []int{0}}}).Validate(); err == nil {
		t.Error("allow_tcp_connect 0 must be rejected")
	}
	// Out-of-range ports are still rejected for open_port.
	if err := (&Profile{Network: Network{OpenPort: []int{70000}}}).Validate(); err == nil {
		t.Error("open_port 70000 must be rejected")
	}
}

func TestEffectiveProtectedPaths(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	b := Baseline{ProtectedPaths: []string{"~/.git-credentials", "~/.netrc", "~/.ssh"}}
	got := EffectiveProtectedPaths(b, []string{"~/.git-credentials"})
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	for _, p := range got {
		if strings.Contains(p, ".git-credentials") {
			t.Errorf("override_deny hole not punched: %v", got)
		}
		if !strings.HasPrefix(p, "/home/u/") {
			t.Errorf("protected path not expanded: %q", p)
		}
	}
}

func TestFilterEnv(t *testing.T) {
	environ := []string{
		"HOME=/home/u",
		"PATH=/usr/bin",
		"OMAC_BASE=http://127.0.0.1:1/x",
		"AWS_SECRET_ACCESS_KEY=oops",
		"DYLD_INSERT_LIBRARIES=/evil.dylib",
		"NODE_OPTIONS=--require evil",
		"OP_SESSION_team=tok",
		"HTTP_PROXY=http://old:1",
	}
	injected := map[string]string{"HTTP_PROXY": "http://omac:tok@127.0.0.1:9"}

	// With allowlist.
	got := FilterEnv(environ, []string{"HOME", "PATH", "OMAC_*"}, injected)
	want := map[string]string{
		"HOME":       "/home/u",
		"PATH":       "/usr/bin",
		"OMAC_BASE":  "http://127.0.0.1:1/x",
		"HTTP_PROXY": "http://omac:tok@127.0.0.1:9",
	}
	gotMap := envMap(got)
	if len(gotMap) != len(want) {
		t.Errorf("got %v want %v", gotMap, want)
	}
	for k, v := range want {
		if gotMap[k] != v {
			t.Errorf("%s = %q want %q", k, gotMap[k], v)
		}
	}

	// Without allowlist: everything but the blocklist passes.
	got = FilterEnv(environ, nil, nil)
	gotMap = envMap(got)
	if _, ok := gotMap["AWS_SECRET_ACCESS_KEY"]; !ok {
		t.Error("no allowlist: AWS var should pass")
	}
	for _, k := range []string{"DYLD_INSERT_LIBRARIES", "NODE_OPTIONS", "OP_SESSION_team"} {
		if _, ok := gotMap[k]; ok {
			t.Errorf("blocklisted %s leaked", k)
		}
	}

	// Blocklist beats allowlist.
	got = FilterEnv(environ, []string{"NODE_OPTIONS"}, nil)
	if len(got) != 0 {
		t.Errorf("blocklist must beat allowlist: %v", got)
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}
