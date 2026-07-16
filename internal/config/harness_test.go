package config

import (
	"os"
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

func TestSystemContextArgsClaude(t *testing.T) {
	h, ok := LookupHarness("claude")
	if !ok {
		t.Fatal("claude harness not found")
	}
	if h.SystemContextArgs == nil {
		t.Fatal("claude SystemContextArgs is nil; want a flag builder")
	}
	got := h.SystemContextArgs("BRIEF")
	want := []string{"--append-system-prompt", "BRIEF"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SystemContextArgs = %v; want %v", got, want)
	}
}

func TestSystemContextArgsOpenCodeNil(t *testing.T) {
	h, ok := LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}
	if h.SystemContextArgs != nil {
		t.Error("opencode SystemContextArgs should be nil (no system-prompt flag exists)")
	}
}

func TestSystemContextArgsCodex(t *testing.T) {
	h, ok := LookupHarness("codex")
	if !ok {
		t.Fatal("codex harness not found")
	}
	if h.SystemContextArgs == nil {
		t.Fatal("codex SystemContextArgs is nil; want a config-override builder")
	}
	got := h.SystemContextArgs("BRIEF")
	want := []string{"-c", "instructions=BRIEF"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SystemContextArgs = %v; want %v", got, want)
	}
}

func TestSystemContextArgsCopilotNil(t *testing.T) {
	h, ok := LookupHarness("copilot")
	if !ok {
		t.Fatal("copilot harness not found")
	}
	if h.SystemContextArgs != nil {
		t.Error("copilot SystemContextArgs should be nil (no system-prompt flag exists)")
	}
	if h.BriefingEnvFunc == nil {
		t.Fatal("copilot BriefingEnvFunc is nil; want an env+file builder")
	}
}

func TestBriefingEnvFuncCopilot(t *testing.T) {
	h, ok := LookupHarness("copilot")
	if !ok {
		t.Fatal("copilot harness not found")
	}
	if h.BriefingEnvFunc == nil {
		t.Fatal("copilot BriefingEnvFunc is nil")
	}
	tmp := t.TempDir()
	got := h.BriefingEnvFunc("BRIEF", tmp)
	if got["COPILOT_CUSTOM_INSTRUCTIONS_DIRS"] != tmp {
		t.Errorf("COPILOT_CUSTOM_INSTRUCTIONS_DIRS = %q; want %q", got["COPILOT_CUSTOM_INSTRUCTIONS_DIRS"], tmp)
	}
	data, err := os.ReadFile(filepath.Join(tmp, "AGENTS.md"))
	if err != nil {
		t.Fatalf("AGENTS.md not written: %v", err)
	}
	if string(data) != "BRIEF" {
		t.Errorf("AGENTS.md = %q; want %q", string(data), "BRIEF")
	}
}

func TestSandboxDirsCodex(t *testing.T) {
	h, ok := LookupHarness("codex")
	if !ok {
		t.Fatal("codex harness not found")
	}
	if !reflect.DeepEqual(h.SandboxDirs, []string{"~/.codex", "~/.cache/codex"}) {
		t.Errorf("codex SandboxDirs = %v; want [~/.codex ~/.cache/codex]", h.SandboxDirs)
	}
}

func TestSandboxDirsCopilot(t *testing.T) {
	h, ok := LookupHarness("copilot")
	if !ok {
		t.Fatal("copilot harness not found")
	}
	if !reflect.DeepEqual(h.SandboxDirs, []string{"~/.copilot", "~/.cache/copilot"}) {
		t.Errorf("copilot SandboxDirs = %v; want [~/.copilot ~/.cache/copilot]", h.SandboxDirs)
	}
}

func TestSandboxDirsOpenCode(t *testing.T) {
	h, ok := LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}
	want := []string{"~/.local/share/opencode", "~/.local/state/opencode", "~/.config/opencode", "~/.opencode", "~/.cache/opencode"}
	if !reflect.DeepEqual(h.SandboxDirs, want) {
		t.Errorf("opencode SandboxDirs = %v; want %v", h.SandboxDirs, want)
	}
}

func TestSandboxDirsClaude(t *testing.T) {
	h, ok := LookupHarness("claude")
	if !ok {
		t.Fatal("claude harness not found")
	}
	want := []string{"~/.claude", "~/.local/share/claude", "~/.cache/claude"}
	if !reflect.DeepEqual(h.SandboxDirs, want) {
		t.Errorf("claude SandboxDirs = %v; want %v", h.SandboxDirs, want)
	}
}

// --- Codex + Copilot harness descriptors -------------------------------------

func TestLookupCodexHarness(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{"codex", "codex", true},
		{"Codex", "codex", true},
		{"cx", "codex", true},
		{"CX", "codex", true},
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

func TestLookupCopilotHarness(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{"copilot", "copilot", true},
		{"Copilot", "copilot", true},
		{"co", "copilot", true},
		{"CO", "copilot", true},
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

func TestCodexHarnessDescriptor(t *testing.T) {
	h, ok := LookupHarness("codex")
	if !ok {
		t.Fatal("codex harness not registered")
	}
	if !reflect.DeepEqual(h.InnerCmd, []string{"codex"}) {
		t.Errorf("codex InnerCmd = %v, want [codex]", h.InnerCmd)
	}
	if h.ServerLaunch != nil {
		t.Errorf("codex ServerLaunch = %v, want nil", h.ServerLaunch)
	}
	if h.BridgeDir != ".codex" {
		t.Errorf("codex BridgeDir = %q, want .codex", h.BridgeDir)
	}
	if h.SkillsBase != "codex" {
		t.Errorf("codex SkillsBase = %q, want codex", h.SkillsBase)
	}
	if h.UserConfigHome != ".codex" {
		t.Errorf("codex UserConfigHome = %q, want .codex", h.UserConfigHome)
	}
}

func TestCopilotHarnessDescriptor(t *testing.T) {
	h, ok := LookupHarness("copilot")
	if !ok {
		t.Fatal("copilot harness not registered")
	}
	if !reflect.DeepEqual(h.InnerCmd, []string{"copilot"}) {
		t.Errorf("copilot InnerCmd = %v, want [copilot]", h.InnerCmd)
	}
	if h.ServerLaunch != nil {
		t.Errorf("copilot ServerLaunch = %v, want nil", h.ServerLaunch)
	}
	if h.BridgeDir != ".copilot" {
		t.Errorf("copilot BridgeDir = %q, want .copilot", h.BridgeDir)
	}
	if h.SkillsBase != "copilot" {
		t.Errorf("copilot SkillsBase = %q, want copilot", h.SkillsBase)
	}
	if h.UserConfigHome != ".copilot" {
		t.Errorf("copilot UserConfigHome = %q, want .copilot", h.UserConfigHome)
	}
}

func TestCodexSessionMetadata(t *testing.T) {
	h, ok := LookupHarness("codex")
	if !ok {
		t.Fatal("codex harness not registered")
	}
	if h.Session == nil {
		t.Fatal("codex Session is nil, want session metadata")
	}
	if !reflect.DeepEqual(h.Session.ContinueArgs, []string{"resume", "--last"}) {
		t.Errorf("codex ContinueArgs = %v, want [resume --last]", h.Session.ContinueArgs)
	}
	if got := h.Session.ResumeByIDArgs("abc123"); !reflect.DeepEqual(got, []string{"resume", "abc123"}) {
		t.Errorf("codex ResumeByIDArgs = %v, want [resume abc123]", got)
	}
	if h.Session.ListKind != SessionListCodex {
		t.Errorf("codex ListKind = %v, want SessionListCodex", h.Session.ListKind)
	}
}

func TestCopilotSessionMetadata(t *testing.T) {
	h, ok := LookupHarness("copilot")
	if !ok {
		t.Fatal("copilot harness not registered")
	}
	if h.Session == nil {
		t.Fatal("copilot Session is nil, want session metadata")
	}
	if !reflect.DeepEqual(h.Session.ContinueArgs, []string{"--continue"}) {
		t.Errorf("copilot ContinueArgs = %v, want [--continue]", h.Session.ContinueArgs)
	}
	if got := h.Session.ResumeByIDArgs("abc123"); !reflect.DeepEqual(got, []string{"--session-id", "abc123"}) {
		t.Errorf("copilot ResumeByIDArgs = %v, want [--session-id abc123]", got)
	}
	if h.Session.ListKind != SessionListCopilot {
		t.Errorf("copilot ListKind = %v, want SessionListCopilot", h.Session.ListKind)
	}
}

func TestCodexWorkdirSkillsDir(t *testing.T) {
	h, _ := LookupHarness("codex")
	if got := h.WorkdirSkillsDir(); got != ".codex/skills" {
		t.Errorf("codex WorkdirSkillsDir = %q, want .codex/skills", got)
	}
}

func TestCopilotWorkdirSkillsDir(t *testing.T) {
	h, _ := LookupHarness("copilot")
	if got := h.WorkdirSkillsDir(); got != ".copilot/skills" {
		t.Errorf("copilot WorkdirSkillsDir = %q, want .copilot/skills", got)
	}
}

func TestCodexInScopeSkillsBases(t *testing.T) {
	h, _ := LookupHarness("codex")
	if got := h.InScopeSkillsBases(); !reflect.DeepEqual(got, []string{"codex", SharedSkillsBase}) {
		t.Errorf("codex bases = %v, want [codex agents]", got)
	}
}

func TestCopilotInScopeSkillsBases(t *testing.T) {
	h, _ := LookupHarness("copilot")
	if got := h.InScopeSkillsBases(); !reflect.DeepEqual(got, []string{"copilot", SharedSkillsBase}) {
		t.Errorf("copilot bases = %v, want [copilot agents]", got)
	}
}

func TestConfigHomeEnvOverride(t *testing.T) {
	h, _ := LookupHarness("codex")
	t.Setenv("CODEX_HOME", "/tmp/codex-home")
	if got := h.ConfigHome(); got != "/tmp/codex-home" {
		t.Errorf("ConfigHome() = %q, want /tmp/codex-home", got)
	}
}

func TestConfigHomeEnvOverrideUnset(t *testing.T) {
	h, _ := LookupHarness("codex")
	t.Setenv("CODEX_HOME", "")
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".codex")
	if got := h.ConfigHome(); got != want {
		t.Errorf("ConfigHome() = %q, want %q", got, want)
	}
}

func TestConfigHomeEnvOverrideClaude(t *testing.T) {
	h, _ := LookupHarness("claude-code")
	t.Setenv("CLAUDE_HOME", "/tmp/claude-home")
	if got := h.ConfigHome(); got != "/tmp/claude-home" {
		t.Errorf("ConfigHome() = %q, want /tmp/claude-home", got)
	}
}

func TestConfigHomeEnvOverrideOpenCode(t *testing.T) {
	h, _ := LookupHarness("opencode")
	t.Setenv("OPENCODE_HOME", "/tmp/oc-home")
	if got := h.ConfigHome(); got != "/tmp/oc-home" {
		t.Errorf("ConfigHome() = %q, want /tmp/oc-home", got)
	}
}

func TestGlobalSkillsDirEnvOverride(t *testing.T) {
	h, _ := LookupHarness("codex")
	t.Setenv("CODEX_HOME", "/tmp/codex-skills")
	want := "/tmp/codex-skills/skills"
	if got := h.GlobalSkillsDir(); got != want {
		t.Errorf("GlobalSkillsDir() = %q, want %q", got, want)
	}
}

func TestGlobalSkillsDirEnvOverrideClaude(t *testing.T) {
	h, _ := LookupHarness("claude-code")
	t.Setenv("CLAUDE_HOME", "/tmp/claude-skills")
	want := "/tmp/claude-skills/skills"
	if got := h.GlobalSkillsDir(); got != want {
		t.Errorf("GlobalSkillsDir() = %q, want %q", got, want)
	}
}

func TestGlobalSkillsDirEnvOverrideOpenCode(t *testing.T) {
	h, _ := LookupHarness("opencode")
	t.Setenv("OPENCODE_HOME", "/tmp/oc-skills")
	want := "/tmp/oc-skills/skills"
	if got := h.GlobalSkillsDir(); got != want {
		t.Errorf("GlobalSkillsDir() = %q, want %q", got, want)
	}
}

// --- Pi harness descriptor ---------------------------------------------------

func TestLookupPiHarness(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{"pi", "pi", true},
		{"Pi", "pi", true},
		{"PI", "pi", true},
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

func TestPiHarnessDescriptor(t *testing.T) {
	h, ok := LookupHarness("pi")
	if !ok {
		t.Fatal("pi harness not registered")
	}
	if !reflect.DeepEqual(h.InnerCmd, []string{"pi"}) {
		t.Errorf("pi InnerCmd = %v, want [pi]", h.InnerCmd)
	}
	if h.ServerLaunch != nil {
		t.Errorf("pi ServerLaunch = %v, want nil", h.ServerLaunch)
	}
	if h.BridgeDir != ".pi/extensions" {
		t.Errorf("pi BridgeDir = %q, want .pi/extensions", h.BridgeDir)
	}
	if h.SkillsBase != "pi" {
		t.Errorf("pi SkillsBase = %q, want pi", h.SkillsBase)
	}
	if want := filepath.Join(".pi", "agent"); h.UserConfigHome != want {
		t.Errorf("pi UserConfigHome = %q, want %q", h.UserConfigHome, want)
	}
	if h.HomeEnv != "PI_CODING_AGENT_DIR" {
		t.Errorf("pi HomeEnv = %q, want PI_CODING_AGENT_DIR", h.HomeEnv)
	}
}

// TestPiConfigHome guards the ~/.pi/agent (not ~/.pi) config home: pi's own
// docs and a live install confirm models.json, sessions/, skills/, and
// extensions/ all live under ~/.pi/agent/, not directly under ~/.pi/. This
// is the property GlobalSkillsDir/GlobalBridgeDir/piSessionsRoot all derive
// from, so a regression here silently breaks skill/bridge/session discovery
// for pi without any single test catching it directly.
func TestPiConfigHome(t *testing.T) {
	h, _ := LookupHarness("pi")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".pi", "agent"); h.ConfigHome() != want {
		t.Errorf("pi ConfigHome() = %q, want %q", h.ConfigHome(), want)
	}
}

func TestPiConfigHomeEnvOverride(t *testing.T) {
	h, _ := LookupHarness("pi")
	t.Setenv("PI_CODING_AGENT_DIR", "/custom/pi/agent")
	if got := h.ConfigHome(); got != "/custom/pi/agent" {
		t.Errorf("pi ConfigHome() with PI_CODING_AGENT_DIR set = %q, want /custom/pi/agent", got)
	}
}

func TestPiGlobalSkillsDir(t *testing.T) {
	h, _ := LookupHarness("pi")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".pi", "agent", "skills"); h.GlobalSkillsDir() != want {
		t.Errorf("pi GlobalSkillsDir() = %q, want %q", h.GlobalSkillsDir(), want)
	}
}

func TestPiGlobalBridgeDir(t *testing.T) {
	h, _ := LookupHarness("pi")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".pi", "agent", "extensions"); h.GlobalBridgeDir() != want {
		t.Errorf("pi GlobalBridgeDir() = %q, want %q", h.GlobalBridgeDir(), want)
	}
}

func TestPiSessionMetadata(t *testing.T) {
	h, ok := LookupHarness("pi")
	if !ok {
		t.Fatal("pi harness not registered")
	}
	if h.Session == nil {
		t.Fatal("pi Session is nil, want session metadata")
	}
	if !reflect.DeepEqual(h.Session.ContinueArgs, []string{"-c"}) {
		t.Errorf("pi ContinueArgs = %v, want [-c]", h.Session.ContinueArgs)
	}
	if got := h.Session.ResumeByIDArgs("abc123"); !reflect.DeepEqual(got, []string{"--session", "abc123"}) {
		t.Errorf("pi ResumeByIDArgs = %v, want [--session abc123]", got)
	}
	if h.Session.ListKind != SessionListPi {
		t.Errorf("pi ListKind = %v, want SessionListPi", h.Session.ListKind)
	}
}

func TestPiWorkdirSkillsDir(t *testing.T) {
	h, _ := LookupHarness("pi")
	if got := h.WorkdirSkillsDir(); got != ".pi/skills" {
		t.Errorf("pi WorkdirSkillsDir = %q, want .pi/skills", got)
	}
}

func TestPiInScopeSkillsBases(t *testing.T) {
	h, _ := LookupHarness("pi")
	if got := h.InScopeSkillsBases(); !reflect.DeepEqual(got, []string{"pi", SharedSkillsBase}) {
		t.Errorf("pi bases = %v, want [pi agents]", got)
	}
}

func TestPiSandboxDirs(t *testing.T) {
	h, ok := LookupHarness("pi")
	if !ok {
		t.Fatal("pi harness not found")
	}
	if !reflect.DeepEqual(h.SandboxDirs, []string{"~/.pi"}) {
		t.Errorf("pi SandboxDirs = %v, want [~/.pi]", h.SandboxDirs)
	}
}

func TestPiSystemContextArgsNil(t *testing.T) {
	h, ok := LookupHarness("pi")
	if !ok {
		t.Fatal("pi harness not found")
	}
	if h.SystemContextArgs != nil {
		t.Error("pi SystemContextArgs should be nil (no system-prompt flag exists)")
	}
	if h.BriefingEnvFunc != nil {
		t.Error("pi BriefingEnvFunc should be nil (briefing via OMAC_SANDBOX_BRIEFING + TS extension)")
	}
}
