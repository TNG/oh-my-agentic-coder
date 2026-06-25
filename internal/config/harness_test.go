package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLookupHarness(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{"opencode", "opencode", true},
		{"OpenCode", "opencode", true},
		{"oc", "opencode", true},
		{"claude-code", "claude-code", true},
		{"claude", "claude-code", true},
		{"CLAUDE", "claude-code", true},
		{"cc", "claude-code", true},
		{"  claude  ", "claude-code", true},
		{"", "", false},
		{"nope", "", false},
		{"claud", "", false},
	}
	for _, c := range cases {
		h, ok := LookupHarness(c.in)
		if ok != c.wantOK {
			t.Errorf("LookupHarness(%q) ok=%v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && h.Name != c.wantName {
			t.Errorf("LookupHarness(%q) name=%q, want %q", c.in, h.Name, c.wantName)
		}
	}
}

func TestDefaultHarnessIsOpenCode(t *testing.T) {
	if got := DefaultHarness().Name; got != "opencode" {
		t.Fatalf("DefaultHarness() = %q, want opencode", got)
	}
}

func TestHarnessAliasesAreUnique(t *testing.T) {
	seen := map[string]string{} // token -> owner
	for _, h := range harnessRegistry() {
		tokens := append([]string{h.Name}, h.Aliases...)
		for _, tok := range tokens {
			key := strings.ToLower(tok)
			if owner, dup := seen[key]; dup {
				t.Errorf("token %q is claimed by both %q and %q", tok, owner, h.Name)
			}
			seen[key] = h.Name
		}
	}
}

func TestIsHarnessName(t *testing.T) {
	for _, tok := range []string{"opencode", "claude", "cc", "oc"} {
		if !IsHarnessName(tok) {
			t.Errorf("IsHarnessName(%q) = false, want true", tok)
		}
	}
	for _, tok := range []string{"", "bash", "--verbose", "claud"} {
		if IsHarnessName(tok) {
			t.Errorf("IsHarnessName(%q) = true, want false", tok)
		}
	}
}

func TestUnknownHarnessErrorListsNames(t *testing.T) {
	err := UnknownHarnessError("zzz")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, name := range HarnessNames() {
		if !strings.Contains(msg, name) {
			t.Errorf("error %q does not mention supported harness %q", msg, name)
		}
	}
}

func TestApplyServerLaunch(t *testing.T) {
	oc, _ := LookupHarness("opencode")
	cc, _ := LookupHarness("claude-code")
	cases := []struct {
		name     string
		h        Harness
		inner    []string
		trailing []string
		want     []string
	}{
		{"opencode bare -> serve", oc, []string{"opencode"}, nil, []string{"opencode", "serve"}},
		{"opencode flags only -> serve", oc, []string{"opencode"}, []string{"--port", "0"}, []string{"opencode", "serve"}},
		{"opencode already serve", oc, []string{"opencode", "serve"}, nil, []string{"opencode", "serve"}},
		{"opencode other subcommand in trailing", oc, []string{"opencode"}, []string{"run", "x"}, []string{"opencode"}},
		{"opencode inner tail subcommand", oc, []string{"opencode", "tui"}, nil, []string{"opencode", "tui"}},
		{"opencode inner tail flag -> insert", oc, []string{"opencode", "--pure"}, nil, []string{"opencode", "serve", "--pure"}},
		// The opencode harness applies its server-launch to whatever the
		// inner executable is (it is keyed on the harness, not the basename):
		{"opencode harness with overridden exe", oc, []string{"/opt/oc"}, nil, []string{"/opt/oc", "serve"}},
		// Claude Code has no server-launch convention -> unchanged.
		{"claude unchanged", cc, []string{"claude"}, nil, []string{"claude"}},
		{"claude with args unchanged", cc, []string{"claude", "--model", "x"}, nil, []string{"claude", "--model", "x"}},
		{"empty inner", oc, nil, nil, nil},
	}
	for _, c := range cases {
		got := c.h.ApplyServerLaunch(append([]string(nil), c.inner...), c.trailing)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ApplyServerLaunch(%v, %v) = %v, want %v", c.name, c.inner, c.trailing, got, c.want)
		}
	}
}

func TestResolveInnerCmd(t *testing.T) {
	oc, _ := LookupHarness("opencode")
	cc, _ := LookupHarness("claude-code")
	cases := []struct {
		name         string
		h            Harness
		profileInner []string
		override     string
		want         []string
	}{
		{"opencode default", oc, nil, "", []string{"opencode"}},
		{"claude default", cc, nil, "", []string{"claude"}},
		{"profile inner wins over harness default", oc, []string{"myagent", "--x"}, "", []string{"myagent", "--x"}},
		{"override replaces exe, no profile", cc, nil, "claude-dev", []string{"claude-dev"}},
		{"override replaces exe, keeps profile args", oc, []string{"opencode", "--flag"}, "oc2", []string{"oc2", "--flag"}},
	}
	for _, c := range cases {
		got := c.h.ResolveInnerCmd(c.profileInner, c.override)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ResolveInnerCmd(%v, %q) = %v, want %v", c.name, c.profileInner, c.override, got, c.want)
		}
	}
}

func TestDefaultSandboxProfilesHaveEmptyInnerCmd(t *testing.T) {
	// The sandboxed profiles must NOT bake an inner_cmd: the harness supplies
	// it at launch. Baking one here would make `omac start claude` silently
	// run the baked command instead of Claude.
	lc := DefaultLauncherConfig()
	for _, name := range []string{"nono", "nono-netprofile"} {
		prof, ok := lc.Sandbox.Profiles[name]
		if !ok {
			t.Fatalf("%s profile missing", name)
		}
		if len(prof.InnerCmd) != 0 {
			t.Errorf("%s inner_cmd = %v, want empty (harness supplies it)", name, prof.InnerCmd)
		}
		// The sandbox command template must remain harness-independent.
		if joined := strings.Join(prof.Command, " "); !strings.Contains(joined, "{{inner_cmd}}") {
			t.Errorf("%s command lost {{inner_cmd}} placeholder: %v", name, prof.Command)
		}
	}
	// no-sandbox-debug is a debug shell, not an agent harness: it keeps bash.
	if got := lc.Sandbox.Profiles["no-sandbox-debug"].InnerCmd; !reflect.DeepEqual(got, []string{"bash"}) {
		t.Errorf("no-sandbox-debug inner_cmd = %v, want [bash]", got)
	}
}

func TestHarnessSuppliesInnerForEmptyProfile(t *testing.T) {
	// With the default (empty) profile inner_cmd, the harness default is used.
	oc, _ := LookupHarness("opencode")
	cc, _ := LookupHarness("claude-code")
	lc := DefaultLauncherConfig()
	prof := lc.Sandbox.Profiles["nono"]
	if got := oc.ResolveInnerCmd(prof.InnerCmd, ""); !reflect.DeepEqual(got, []string{"opencode"}) {
		t.Errorf("opencode harness inner = %v, want [opencode]", got)
	}
	if got := cc.ResolveInnerCmd(prof.InnerCmd, ""); !reflect.DeepEqual(got, []string{"claude"}) {
		t.Errorf("claude harness inner = %v, want [claude]", got)
	}
}

func TestInScopeSkillsBases(t *testing.T) {
	oc, _ := LookupHarness("opencode")
	cc, _ := LookupHarness("claude-code")
	if got := oc.InScopeSkillsBases(); !reflect.DeepEqual(got, []string{"opencode", SharedSkillsBase}) {
		t.Errorf("opencode bases = %v, want [opencode agents]", got)
	}
	if got := cc.InScopeSkillsBases(); !reflect.DeepEqual(got, []string{"claude", SharedSkillsBase}) {
		t.Errorf("claude bases = %v, want [claude agents]", got)
	}
}

func TestSkillsBaseInScope(t *testing.T) {
	oc, _ := LookupHarness("opencode")
	cc, _ := LookupHarness("claude-code")
	if !oc.SkillsBaseInScope("opencode") || !oc.SkillsBaseInScope(SharedSkillsBase) || oc.SkillsBaseInScope("claude") {
		t.Error("opencode scope wrong")
	}
	if !cc.SkillsBaseInScope("claude") || !cc.SkillsBaseInScope(SharedSkillsBase) || cc.SkillsBaseInScope("opencode") {
		t.Error("claude scope wrong")
	}
}

func TestWorkdirSkillsDir(t *testing.T) {
	oc, _ := LookupHarness("opencode")
	cc, _ := LookupHarness("claude-code")
	if got := oc.WorkdirSkillsDir(); got != ".opencode/skills" {
		t.Errorf("opencode WorkdirSkillsDir = %q, want .opencode/skills", got)
	}
	if got := cc.WorkdirSkillsDir(); got != ".claude/skills" {
		t.Errorf("claude WorkdirSkillsDir = %q, want .claude/skills", got)
	}
}

func TestGlobalBridgeDir(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	oc, _ := LookupHarness("opencode")
	want := filepath.Join(xdg, "opencode", "plugins")
	if got := oc.GlobalBridgeDir(); got != want {
		t.Errorf("opencode GlobalBridgeDir = %q, want %q", got, want)
	}

	// Claude Code's bridge dir (".claude") is its config base with no
	// nested plugin leaf, so global bridge installation is not modeled.
	cc, _ := LookupHarness("claude-code")
	if got := cc.GlobalBridgeDir(); got != "" {
		t.Errorf("claude GlobalBridgeDir = %q, want empty", got)
	}
}

func TestHarnessSessionMetadata(t *testing.T) {
	oc, _ := LookupHarness("opencode")
	if oc.Session == nil {
		t.Fatal("opencode Session is nil, want session metadata")
	}
	if !reflect.DeepEqual(oc.Session.ContinueArgs, []string{"--continue"}) {
		t.Errorf("opencode ContinueArgs = %v, want [--continue]", oc.Session.ContinueArgs)
	}
	if got := oc.Session.ResumeByIDArgs("ses_X"); !reflect.DeepEqual(got, []string{"--session", "ses_X"}) {
		t.Errorf("opencode ResumeByIDArgs = %v, want [--session ses_X]", got)
	}
	if oc.Session.ListKind != SessionListOpenCodeCLI {
		t.Errorf("opencode ListKind = %v, want SessionListOpenCodeCLI", oc.Session.ListKind)
	}

	cc, _ := LookupHarness("claude-code")
	if cc.Session == nil {
		t.Fatal("claude Session is nil, want session metadata")
	}
	if !reflect.DeepEqual(cc.Session.ContinueArgs, []string{"--continue"}) {
		t.Errorf("claude ContinueArgs = %v, want [--continue]", cc.Session.ContinueArgs)
	}
	if got := cc.Session.ResumeByIDArgs("abc-123"); !reflect.DeepEqual(got, []string{"--resume", "abc-123"}) {
		t.Errorf("claude ResumeByIDArgs = %v, want [--resume abc-123]", got)
	}
	if cc.Session.ListKind != SessionListClaudeFiles {
		t.Errorf("claude ListKind = %v, want SessionListClaudeFiles", cc.Session.ListKind)
	}
}

// TestHarnessSessionNilIsSafe documents that a harness with no Session block
// (the zero default for any future descriptor) is tolerated: callers must
// nil-check before using session metadata.
func TestHarnessSessionNilIsSafe(t *testing.T) {
	var h Harness // zero value: Session == nil
	if h.Session != nil {
		t.Fatal("zero Harness should have nil Session")
	}
}
