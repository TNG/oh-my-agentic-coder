package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
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

// TestServerLaunchListenPort locks the per-harness server listen port that
// omac serve must allowlist in the sandbox. opencode's `serve` daemon binds
// 4096 by default; without this the bind is denied under a restrictive
// profile (issue #115). Harnesses with no server mode declare no port.
func TestServerLaunchListenPort(t *testing.T) {
	oc, ok := LookupHarness("opencode")
	if !ok || oc.ServerLaunch == nil {
		t.Fatal("opencode must have a ServerLaunch")
	}
	if oc.ServerLaunch.ListenPort != 4096 {
		t.Errorf("opencode ServerLaunch.ListenPort = %d, want 4096", oc.ServerLaunch.ListenPort)
	}
	// opencode's server is unauthenticated unless OPENCODE_SERVER_PASSWORD is
	// set; declaring it lets omac warn when the exposed loopback port is open.
	if oc.ServerLaunch.AuthEnvVar != "OPENCODE_SERVER_PASSWORD" {
		t.Errorf("opencode ServerLaunch.AuthEnvVar = %q, want OPENCODE_SERVER_PASSWORD", oc.ServerLaunch.AuthEnvVar)
	}
	for _, name := range []string{"claude-code", "codex", "copilot", "pi", "codewhale"} {
		h, _ := LookupHarness(name)
		if h.ServerLaunch != nil {
			t.Errorf("%s unexpectedly declares a ServerLaunch (%+v)", name, h.ServerLaunch)
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

func TestHarnessSandboxEnvAllowIsSafe(t *testing.T) {
	// A harness may auto-forward the provider-auth vars it unambiguously
	// needs (injected via --allow-env for the selected harness); the list is
	// allowed to be empty for multi-provider harnesses that must not blindly
	// forward a grab-bag of third-party keys (declare them in allow_vars
	// instead). Whatever IS declared must be a real, non-blocklisted name —
	// a blank or always-drop entry would be a silent no-op.
	for _, h := range AllHarnesses() {
		for _, name := range h.SandboxEnvAllow {
			if strings.TrimSpace(name) == "" {
				t.Errorf("harness %q has an empty SandboxEnvAllow entry", h.Name)
			}
			if sandboxprofile.IsDangerousEnvVar(name) {
				t.Errorf("harness %q forwards blocklisted env var %q (would be stripped)", h.Name, name)
			}
		}
	}
}

// TestHarnessAuthForwardPolicy pins the auto-forward policy: single-provider
// harnesses inject their targeted auth vars; multi-provider harnesses
// (opencode, pi) auto-forward NOTHING — a user relying on env-based provider
// auth declares the specific key in the profile's allow_vars, so omac does not
// blindly push every third-party key into the sandbox.
func TestHarnessAuthForwardPolicy(t *testing.T) {
	multiProvider := map[string]bool{"opencode": true, "pi": true, "codewhale": true}
	for _, h := range AllHarnesses() {
		if multiProvider[h.Name] {
			if len(h.SandboxEnvAllow) != 0 {
				t.Errorf("multi-provider harness %q must not auto-forward provider keys; got %v", h.Name, h.SandboxEnvAllow)
			}
			continue
		}
	}
	// Single-provider harnesses keep their targeted forwarding.
	for name, want := range map[string]string{
		"claude-code": "ANTHROPIC_API_KEY",
		"codex":       "OPENAI_API_KEY",
		"copilot":     "GITHUB_TOKEN",
	} {
		h, ok := LookupHarness(name)
		if !ok {
			t.Errorf("harness %q not found", name)
			continue
		}
		found := false
		for _, v := range h.SandboxEnvAllow {
			if v == want {
				found = true
			}
		}
		if !found {
			t.Errorf("single-provider harness %q should forward %q; got %v", name, want, h.SandboxEnvAllow)
		}
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
	if !reflect.DeepEqual(h.SandboxDirs, []string{"~/.codex"}) {
		t.Errorf("codex SandboxDirs = %v; want [~/.codex]", h.SandboxDirs)
	}
}

func TestSandboxDirsCopilot(t *testing.T) {
	h, ok := LookupHarness("copilot")
	if !ok {
		t.Fatal("copilot harness not found")
	}
	if !reflect.DeepEqual(h.SandboxDirs, []string{"~/.copilot"}) {
		t.Errorf("copilot SandboxDirs = %v; want [~/.copilot]", h.SandboxDirs)
	}
}

func TestSandboxDirsOpenCode(t *testing.T) {
	h, ok := LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}
	want := []string{"~/.local/share/opencode", "~/.local/state/opencode", "~/.config/opencode", "~/.opencode"}
	if !reflect.DeepEqual(h.SandboxDirs, want) {
		t.Errorf("opencode SandboxDirs = %v; want %v", h.SandboxDirs, want)
	}
}

func TestSandboxDirsClaude(t *testing.T) {
	h, ok := LookupHarness("claude")
	if !ok {
		t.Fatal("claude harness not found")
	}
	want := []string{"~/.claude", "~/.local/share/claude"}
	if !reflect.DeepEqual(h.SandboxDirs, want) {
		t.Errorf("claude SandboxDirs = %v; want %v", h.SandboxDirs, want)
	}
}

// Without a redirect the grants are the declared ones, untouched.
func TestResolvedSandboxDirsClaudeNoOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h, _ := LookupHarness("claude-code")
	want := []string{"~/.claude", "~/.local/share/claude"}
	if got := h.ResolvedSandboxDirs(); !reflect.DeepEqual(got, want) {
		t.Errorf("ResolvedSandboxDirs() = %v; want %v", got, want)
	}
}

// A CLAUDE_CONFIG_DIR redirect must move the config-home grant with it —
// otherwise the sandbox denies the credentials the harness actually reads and
// Claude Code prompts for a fresh login.
func TestResolvedSandboxDirsClaudeFollowsConfigDirOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".work-claude"))
	h, _ := LookupHarness("claude-code")

	want := []string{filepath.Join(home, ".work-claude"), "~/.local/share/claude"}
	if got := h.ResolvedSandboxDirs(); !reflect.DeepEqual(got, want) {
		t.Errorf("ResolvedSandboxDirs() = %v; want %v", got, want)
	}
}

// The redirected home replaces the default rather than joining it: granting
// both would expose the very login the user separated out.
func TestResolvedSandboxDirsClaudeDropsDefaultHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".work-claude"))
	h, _ := LookupHarness("claude-code")

	for _, d := range h.ResolvedSandboxDirs() {
		if d == "~/.claude" || d == filepath.Join(home, ".claude") {
			t.Errorf("ResolvedSandboxDirs() still grants the default config home %q", d)
		}
	}
}

// A value that merely SPELLS the default home differently is not a redirect.
// Treating it as one made discovery supersede the default skills root with
// itself and drop it (see TestSources_TrailingSlashKeepsDefaultRoot).
func TestResolvedSandboxDirsClaudeIgnoresNonCanonicalDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := []string{"~/.claude", "~/.local/share/claude"}
	for _, spelling := range []string{
		filepath.Join(home, ".claude") + "/",
		filepath.Join(home, ".config", "..", ".claude"),
		"~/.claude",
	} {
		t.Run(spelling, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", spelling)
			h, _ := LookupHarness("claude-code")
			if got := h.ResolvedSandboxDirs(); !reflect.DeepEqual(got, want) {
				t.Errorf("ResolvedSandboxDirs() = %v; want %v (not a redirect)", got, want)
			}
		})
	}
}

// EVERY harness with a HomeEnv must forward it. ResolvedSandboxDirs is generic,
// so it swaps the grant for all of them; a harness that then cannot see its own
// HomeEnv reads the default home omac just stopped granting — i.e. declaring the
// swap without the forward is worse than not swapping at all.
func TestForwardedEnvVarsCarryHomeEnv(t *testing.T) {
	for _, h := range AllHarnesses() {
		if h.HomeEnv == "" {
			continue
		}
		fwd := h.ForwardedEnvVars()
		found := false
		for _, v := range fwd {
			if v == h.HomeEnv {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s ForwardedEnvVars() = %v; want it to carry %s", h.Name, fwd, h.HomeEnv)
		}
		// The declared auth vars must survive alongside it.
		for _, want := range h.SandboxEnvAllow {
			if !slices.Contains(fwd, want) {
				t.Errorf("%s ForwardedEnvVars() = %v; dropped declared var %s", h.Name, fwd, want)
			}
		}
	}
}

// ForwardedEnvVars must not mutate the descriptor's own slice — the registry is
// rebuilt per call, but a caller appending into shared backing storage is the
// kind of bug that only shows up under a second harness lookup.
func TestForwardedEnvVarsDoesNotMutateSandboxEnvAllow(t *testing.T) {
	h, _ := LookupHarness("claude-code")
	before := append([]string(nil), h.SandboxEnvAllow...)
	_ = h.ForwardedEnvVars()
	if !reflect.DeepEqual(h.SandboxEnvAllow, before) {
		t.Errorf("SandboxEnvAllow mutated: %v -> %v", before, h.SandboxEnvAllow)
	}
}

// Codex is the regression case: its config home IS its only declared sandbox
// dir, so the generic swap drops ~/.codex. Without CODEX_HOME forwarded, codex
// would read ~/.codex — now denied — and lose a login that used to work.
func TestResolvedSandboxDirsCodexFollowsHomeEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".work-codex"))
	h, _ := LookupHarness("codex")

	want := []string{filepath.Join(home, ".work-codex")}
	if got := h.ResolvedSandboxDirs(); !reflect.DeepEqual(got, want) {
		t.Errorf("ResolvedSandboxDirs() = %v; want %v", got, want)
	}
	if !slices.Contains(h.ForwardedEnvVars(), "CODEX_HOME") {
		t.Errorf("ForwardedEnvVars() = %v; want CODEX_HOME so codex reads the granted dir", h.ForwardedEnvVars())
	}
}

// pi declares ~/.pi while its config home is the nested ~/.pi/agent, so no
// entry matches and there is nothing to swap. The redirect target must still be
// granted: PI_CODING_AGENT_DIR is forwarded, so pi reads it.
func TestResolvedSandboxDirsPiAppendsRedirectTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, ".work-pi"))
	h, _ := LookupHarness("pi")

	want := []string{"~/.pi", filepath.Join(home, ".work-pi")}
	if got := h.ResolvedSandboxDirs(); !reflect.DeepEqual(got, want) {
		t.Errorf("ResolvedSandboxDirs() = %v; want %v", got, want)
	}
}

// DefaultGlobalSkillsDir stays on the default home so discovery can tell which
// candidate root a redirect supersedes.
func TestDefaultGlobalSkillsDirIgnoresRedirect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".work-claude"))
	h, _ := LookupHarness("claude-code")

	if want := filepath.Join(home, ".claude", "skills"); h.DefaultGlobalSkillsDir() != want {
		t.Errorf("DefaultGlobalSkillsDir() = %q; want %q", h.DefaultGlobalSkillsDir(), want)
	}
	if want := filepath.Join(home, ".work-claude", "skills"); h.GlobalSkillsDir() != want {
		t.Errorf("GlobalSkillsDir() = %q; want %q", h.GlobalSkillsDir(), want)
	}
}

// HomeEnvNames must cover every declared override, since tests neutralize the
// ambient environment through it — a missing name is a silently leaky test.
func TestHomeEnvNamesCoversRegistry(t *testing.T) {
	names := HomeEnvNames()
	for _, h := range AllHarnesses() {
		if h.HomeEnv != "" && !slices.Contains(names, h.HomeEnv) {
			t.Errorf("HomeEnvNames() = %v; missing %s (%s)", names, h.HomeEnv, h.Name)
		}
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("HomeEnvNames() = %v; contains duplicate %s", names, n)
		}
		seen[n] = true
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
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/claude-home")
	if got := h.ConfigHome(); got != "/tmp/claude-home" {
		t.Errorf("ConfigHome() = %q, want /tmp/claude-home", got)
	}
}

// OpenCode declares no HomeEnv, so nothing may relocate its config home.
// OPENCODE_CONFIG_DIR is the plausible mis-wiring: it is only an ADDITIONAL
// config-search dir, so honoring it would move omac's session store, skills
// install dir, and sandbox grants away from the dirs OpenCode actually reads
// (#233).
func TestConfigHomeOpenCodeHasNoOverride(t *testing.T) {
	h, _ := LookupHarness("opencode")
	if h.HomeEnv != "" {
		t.Fatalf("opencode HomeEnv = %q; want empty (OpenCode has no config-home override)", h.HomeEnv)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("OPENCODE_CONFIG_DIR", filepath.Join(home, ".other-opencode"))
	want := filepath.Join(home, ".config", "opencode")

	if got := h.ConfigHome(); got != want {
		t.Errorf("ConfigHome() = %q; want %q — OPENCODE_CONFIG_DIR must not relocate it", got, want)
	}
	if got, wantSkills := h.GlobalSkillsDir(), filepath.Join(want, "skills"); got != wantSkills {
		t.Errorf("GlobalSkillsDir() = %q; want %q", got, wantSkills)
	}
	// The grants must keep naming the dirs OpenCode really reads.
	if got := h.ResolvedSandboxDirs(); !reflect.DeepEqual(got, h.SandboxDirs) {
		t.Errorf("ResolvedSandboxDirs() = %v; want the declared %v", got, h.SandboxDirs)
	}
}

// $XDG_CONFIG_HOME does move OpenCode's config home — that is the supported
// mechanism, and it is already forwarded into the sandbox.
func TestConfigHomeOpenCodeFollowsXDG(t *testing.T) {
	h, _ := LookupHarness("opencode")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-oc")
	if got, want := h.ConfigHome(), filepath.Join("/tmp/xdg-oc", "opencode"); got != want {
		t.Errorf("ConfigHome() = %q, want %q", got, want)
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
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/claude-skills")
	want := "/tmp/claude-skills/skills"
	if got := h.GlobalSkillsDir(); got != want {
		t.Errorf("GlobalSkillsDir() = %q, want %q", got, want)
	}
}

// OpenCode's global skills dir follows $XDG_CONFIG_HOME, not a HomeEnv
// override (see TestConfigHomeOpenCodeHasNoOverride).
func TestGlobalSkillsDirOpenCodeFollowsXDG(t *testing.T) {
	h, _ := LookupHarness("opencode")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "/tmp/oc-skills")
	want := "/tmp/oc-skills/opencode/skills"
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

// --- CodeWhale harness descriptor --------------------------------------------

func TestLookupCodewhaleHarness(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{"codewhale", "codewhale", true},
		{"CodeWhale", "codewhale", true},
		{"cw", "codewhale", true},
		{"CW", "codewhale", true},
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

func TestCodewhaleHarnessDescriptor(t *testing.T) {
	h, ok := LookupHarness("codewhale")
	if !ok {
		t.Fatal("codewhale harness not registered")
	}
	if !reflect.DeepEqual(h.InnerCmd, []string{"codewhale"}) {
		t.Errorf("codewhale InnerCmd = %v, want [codewhale]", h.InnerCmd)
	}
	if h.ServerLaunch != nil {
		t.Errorf("codewhale ServerLaunch = %v, want nil", h.ServerLaunch)
	}
	// CodeWhale owns the shared "agents" base, not a "codewhale" base:
	// CodeWhale reads workspace .agents/skills, not .codewhale/skills.
	if h.SkillsBase != SharedSkillsBase {
		t.Errorf("codewhale SkillsBase = %q, want %q", h.SkillsBase, SharedSkillsBase)
	}
	if got := h.WorkdirSkillsDir(); got != ".agents/skills" {
		t.Errorf("codewhale WorkdirSkillsDir = %q, want .agents/skills", got)
	}
	if h.UserConfigHome != ".codewhale" {
		t.Errorf("codewhale UserConfigHome = %q, want .codewhale", h.UserConfigHome)
	}
	if h.HomeEnv != "CODEWHALE_HOME" {
		t.Errorf("codewhale HomeEnv = %q, want CODEWHALE_HOME", h.HomeEnv)
	}
	if !reflect.DeepEqual(h.SandboxDirs, []string{"~/.codewhale"}) {
		t.Errorf("codewhale SandboxDirs = %v, want [~/.codewhale]", h.SandboxDirs)
	}
	// Multi-provider: omac auto-forwards no provider keys (see
	// TestHarnessAuthForwardPolicy).
	if len(h.SandboxEnvAllow) != 0 {
		t.Errorf("codewhale SandboxEnvAllow = %v, want empty (multi-provider)", h.SandboxEnvAllow)
	}
}

func TestCodewhaleSessionMetadata(t *testing.T) {
	h, ok := LookupHarness("codewhale")
	if !ok {
		t.Fatal("codewhale harness not registered")
	}
	if h.Session == nil {
		t.Fatal("codewhale Session is nil, want session metadata")
	}
	if !reflect.DeepEqual(h.Session.ContinueArgs, []string{"--continue"}) {
		t.Errorf("codewhale ContinueArgs = %v, want [--continue]", h.Session.ContinueArgs)
	}
	// resume is a top-level subcommand, not a --resume flag (see descriptor).
	if got := h.Session.ResumeByIDArgs("abc123"); !reflect.DeepEqual(got, []string{"resume", "abc123"}) {
		t.Errorf("codewhale ResumeByIDArgs = %v, want [resume abc123]", got)
	}
	if h.Session.ListKind != SessionListCodewhale {
		t.Errorf("codewhale ListKind = %v, want SessionListCodewhale", h.Session.ListKind)
	}
}

func TestCodewhaleConfigHome(t *testing.T) {
	h, _ := LookupHarness("codewhale")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".codewhale"); h.ConfigHome() != want {
		t.Errorf("codewhale ConfigHome() = %q, want %q", h.ConfigHome(), want)
	}
}

func TestCodewhaleConfigHomeEnvOverride(t *testing.T) {
	h, _ := LookupHarness("codewhale")
	t.Setenv("CODEWHALE_HOME", "/custom/codewhale")
	if got := h.ConfigHome(); got != "/custom/codewhale" {
		t.Errorf("codewhale ConfigHome() with CODEWHALE_HOME set = %q, want /custom/codewhale", got)
	}
}

// TestCodewhaleBriefingFileFunc verifies the file-based briefing delivery:
// it writes .codewhale/rules/omac-sandbox-briefing.md (loaded additively and
// unconditionally by CodeWhale, unlike the shadowable .codewhale/instructions.md)
// and returns that workdir-relative path so the launcher can clean it up.
func TestCodewhaleBriefingFileFunc(t *testing.T) {
	h, ok := LookupHarness("codewhale")
	if !ok {
		t.Fatal("codewhale harness not registered")
	}
	if h.SystemContextArgs != nil {
		t.Error("codewhale SystemContextArgs should be nil (no system-prompt flag exists)")
	}
	if h.BriefingEnvFunc != nil {
		t.Error("codewhale BriefingEnvFunc should be nil (briefing via a rules file)")
	}
	if h.BriefingFileFunc == nil {
		t.Fatal("codewhale BriefingFileFunc is nil; want a workdir file writer")
	}
	workdir := t.TempDir()
	rel, err := h.BriefingFileFunc("BRIEF", workdir)
	if err != nil {
		t.Fatalf("BriefingFileFunc error: %v", err)
	}
	want := filepath.Join(".codewhale", "rules", "omac-sandbox-briefing.md")
	if rel != want {
		t.Errorf("BriefingFileFunc rel = %q, want %q", rel, want)
	}
	data, err := os.ReadFile(filepath.Join(workdir, rel))
	if err != nil {
		t.Fatalf("briefing file not written: %v", err)
	}
	if string(data) != "BRIEF" {
		t.Errorf("briefing file = %q, want %q", string(data), "BRIEF")
	}
}
