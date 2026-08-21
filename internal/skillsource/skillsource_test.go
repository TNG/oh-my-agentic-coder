package skillsource

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

func ocHarness(t *testing.T) config.Harness {
	t.Helper()
	h, ok := config.LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness not registered")
	}
	return h
}

func ccHarness(t *testing.T) config.Harness {
	t.Helper()
	h, ok := config.LookupHarness("claude-code")
	if !ok {
		t.Fatal("claude-code harness not registered")
	}
	return h
}

// withFakeHome installs a temporary HOME for the duration of the test and
// points XDG_CONFIG_HOME at $HOME/.config.
func withFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

// stageSkill drops an omac.yaml under <root>/<name>/ so Discover and Resolve
// count it as a skill.
func stageSkill(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "omac.yaml"), []byte("name: "+name+"\n"), 0o644); err != nil {
		t.Fatalf("write omac.yaml: %v", err)
	}
	return dir
}

// ---- Sources: harness scoping ----

func TestSources_OpenCodeScope(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	got := Sources(wd, ocHarness(t))
	// Workdir roots: own base (.opencode) first, then shared (.agents).
	wantFirst := filepath.Join(wd, ".opencode", "skills")
	wantSecond := filepath.Join(wd, ".agents", "skills")
	if got[0].Root != wantFirst || got[0].Kind != "workdir" {
		t.Errorf("source[0] = %+v, want %q", got[0], wantFirst)
	}
	if got[1].Root != wantSecond || got[1].Kind != "workdir" {
		t.Errorf("source[1] = %+v, want %q", got[1], wantSecond)
	}
	// The Claude dir must NEVER appear in OpenCode scope.
	for _, s := range got {
		if s.Root == filepath.Join(wd, ".claude", "skills") {
			t.Errorf("opencode scope must not include .claude/skills; got %+v", got)
		}
	}
}

func TestSources_ClaudeScope(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	got := Sources(wd, ccHarness(t))
	wantFirst := filepath.Join(wd, ".claude", "skills")
	wantSecond := filepath.Join(wd, ".agents", "skills")
	if got[0].Root != wantFirst {
		t.Errorf("source[0] = %+v, want %q", got[0], wantFirst)
	}
	if got[1].Root != wantSecond {
		t.Errorf("source[1] = %+v, want %q", got[1], wantSecond)
	}
	// The OpenCode dir must NEVER appear in Claude scope.
	for _, s := range got {
		if s.Root == filepath.Join(wd, ".opencode", "skills") {
			t.Errorf("claude scope must not include .opencode/skills; got %+v", got)
		}
	}
}

func TestSources_OwnBaseRanksAboveShared(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	got := Sources(wd, ocHarness(t))
	var ownIdx, sharedIdx = -1, -1
	for i, s := range got {
		switch s.Root {
		case filepath.Join(wd, ".opencode", "skills"):
			ownIdx = i
		case filepath.Join(wd, ".agents", "skills"):
			sharedIdx = i
		}
	}
	if ownIdx == -1 || sharedIdx == -1 || ownIdx >= sharedIdx {
		t.Errorf("own base must rank above shared; ownIdx=%d sharedIdx=%d (%+v)", ownIdx, sharedIdx, got)
	}
}

func TestSources_OmitsMissingUserGlobal(t *testing.T) {
	withFakeHome(t)
	got := Sources(t.TempDir(), ocHarness(t))
	for _, s := range got {
		if s.Kind == "user-global" {
			t.Errorf("no user-global source should appear when none exists; got %+v", got)
		}
	}
}

func TestSources_GlobalScopedToHarness(t *testing.T) {
	home := withFakeHome(t)
	// Create both global opencode and global claude roots.
	ocRoot := filepath.Join(home, ".config", "opencode", "skills")
	ccRoot := filepath.Join(home, ".config", "claude", "skills")
	if err := os.MkdirAll(ocRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ccRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	got := Sources(t.TempDir(), ocHarness(t))
	for _, s := range got {
		if s.Root == ccRoot {
			t.Errorf("opencode global scope must not include claude global root; got %+v", got)
		}
	}
}

// ---- Resolve ----

func TestResolve_WorkdirWinsOverGlobal(t *testing.T) {
	home := withFakeHome(t)
	wd := t.TempDir()
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "echo-rest")
	stageSkill(t, filepath.Join(home, ".config", "opencode", "skills"), "echo-rest")

	dir, src, err := Resolve(wd, ocHarness(t), "echo-rest")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(wd, ".opencode", "skills", "echo-rest")
	if dir != want {
		t.Errorf("Resolve picked %q, want workdir %q", dir, want)
	}
	if src.Kind != "workdir" {
		t.Errorf("source.Kind = %q, want workdir", src.Kind)
	}
}

func TestResolve_OwnBaseWinsOverShared(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "dup")
	stageSkill(t, filepath.Join(wd, ".agents", "skills"), "dup")

	dir, _, err := Resolve(wd, ocHarness(t), "dup")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(wd, ".opencode", "skills", "dup")
	if dir != want {
		t.Errorf("Resolve picked %q, want own-base %q", dir, want)
	}
}

func TestResolve_SharedVisibleToBothHarnesses(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	stageSkill(t, filepath.Join(wd, ".agents", "skills"), "neutral")

	for _, h := range []config.Harness{ocHarness(t), ccHarness(t)} {
		dir, _, err := Resolve(wd, h, "neutral")
		if err != nil {
			t.Fatalf("Resolve under %s: %v", h.Name, err)
		}
		want := filepath.Join(wd, ".agents", "skills", "neutral")
		if dir != want {
			t.Errorf("under %s: picked %q, want %q", h.Name, dir, want)
		}
	}
}

func TestResolve_OtherHarnessDirExcluded(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	// Skill lives only under the OpenCode dir.
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "oc-only")

	// Claude must NOT find it.
	_, _, err := Resolve(wd, ccHarness(t), "oc-only")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("claude scope should not resolve an opencode-only skill; err=%v", err)
	}
	// OpenCode must find it.
	if _, _, err := Resolve(wd, ocHarness(t), "oc-only"); err != nil {
		t.Errorf("opencode scope should resolve its own skill; err=%v", err)
	}
}

func TestResolve_NotFound(t *testing.T) {
	withFakeHome(t)
	_, _, err := Resolve(t.TempDir(), ocHarness(t), "nope")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err should be ErrNotExist, got %v", err)
	}
}

// ---- Discover ----

func TestDiscover_ScopedToHarness(t *testing.T) {
	home := withFakeHome(t)
	wd := t.TempDir()
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "oc-wd")
	stageSkill(t, filepath.Join(wd, ".claude", "skills"), "cc-wd")
	stageSkill(t, filepath.Join(wd, ".agents", "skills"), "shared-wd")
	stageSkill(t, filepath.Join(home, ".config", "opencode", "skills"), "oc-global")
	stageSkill(t, filepath.Join(home, ".config", "claude", "skills"), "cc-global")

	ocGot, err := Discover(wd, ocHarness(t))
	if err != nil {
		t.Fatalf("Discover oc: %v", err)
	}
	ocNames := names(ocGot)
	sort.Strings(ocNames)
	if !reflect.DeepEqual(ocNames, []string{"oc-global", "oc-wd", "shared-wd"}) {
		t.Errorf("opencode discover = %v, want [oc-global oc-wd shared-wd]", ocNames)
	}

	ccGot, err := Discover(wd, ccHarness(t))
	if err != nil {
		t.Fatalf("Discover cc: %v", err)
	}
	ccNames := names(ccGot)
	sort.Strings(ccNames)
	if !reflect.DeepEqual(ccNames, []string{"cc-global", "cc-wd", "shared-wd"}) {
		t.Errorf("claude discover = %v, want [cc-global cc-wd shared-wd]", ccNames)
	}
}

func TestDiscover_SkipsDirsWithoutMeta(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wd, ".opencode", "skills", "_template"), 0o755); err != nil {
		t.Fatal(err)
	}
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "real")
	got, err := Discover(wd, ocHarness(t))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 || got[0].Name != "real" {
		t.Errorf("Discover = %+v, want exactly [real]", got)
	}
}

func TestDiscover_MissingDirsAreNoOp(t *testing.T) {
	withFakeHome(t)
	got, err := Discover(t.TempDir(), ocHarness(t))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != nil {
		t.Errorf("Discover with empty layers = %+v, want nil", got)
	}
}

// ---- Candidates / ambiguity ----

func TestCandidates_SingleMatch(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "solo")
	cs, err := Candidates(wd, ocHarness(t), "solo")
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cs) != 1 {
		t.Fatalf("want 1 candidate, got %d (%+v)", len(cs), cs)
	}
}

func TestCandidates_ScopeAmbiguity(t *testing.T) {
	home := withFakeHome(t)
	wd := t.TempDir()
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "dup")
	stageSkill(t, filepath.Join(home, ".config", "opencode", "skills"), "dup")
	cs, err := Candidates(wd, ocHarness(t), "dup")
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cs) != 2 {
		t.Fatalf("want 2 candidates (workdir+global), got %d (%+v)", len(cs), cs)
	}
}

func TestCandidatesAllHarnesses_HarnessAmbiguity(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "slack")
	stageSkill(t, filepath.Join(wd, ".claude", "skills"), "slack")
	cs, err := CandidatesAllHarnesses(wd, "slack")
	if err != nil {
		t.Fatalf("CandidatesAllHarnesses: %v", err)
	}
	// One per harness.
	harnesses := map[string]struct{}{}
	for _, c := range cs {
		harnesses[c.Harness] = struct{}{}
	}
	if len(harnesses) < 2 {
		t.Errorf("expected >=2 distinct harnesses, got %v (%+v)", harnesses, cs)
	}
}

func TestCandidatesAllHarnesses_SharedCollapses(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	// Only a shared skill: it is in every harness scope but is ONE physical
	// dir, so it must collapse to a single shared-labelled candidate.
	stageSkill(t, filepath.Join(wd, ".agents", "skills"), "neutral")
	cs, err := CandidatesAllHarnesses(wd, "neutral")
	if err != nil {
		t.Fatalf("CandidatesAllHarnesses: %v", err)
	}
	if len(cs) != 1 {
		t.Fatalf("shared skill must collapse to 1 candidate, got %d (%+v)", len(cs), cs)
	}
	if cs[0].Harness != SharedHarnessLabel {
		t.Errorf("shared candidate Harness = %q, want %q", cs[0].Harness, SharedHarnessLabel)
	}
}

// ---- DirInHarnessScope ----

func TestDirInHarnessScope(t *testing.T) {
	oc := ocHarness(t)
	cc := ccHarness(t)
	cases := []struct {
		dir string
		oc  bool
		cc  bool
	}{
		{"/home/u/.config/opencode/skills/x", true, false},
		{"/home/u/.config/claude/skills/x", false, true},
		{"/home/u/.config/agents/skills/x", true, true}, // shared
		{".opencode/skills/x", true, false},             // relative
		{".claude/skills/x", false, true},               // relative
		{"/some/custom/place/x", true, true},            // unrecognized -> in scope for both
	}
	for _, c := range cases {
		if got := DirInHarnessScope(c.dir, oc); got != c.oc {
			t.Errorf("DirInHarnessScope(%q, opencode) = %v, want %v", c.dir, got, c.oc)
		}
		if got := DirInHarnessScope(c.dir, cc); got != c.cc {
			t.Errorf("DirInHarnessScope(%q, claude) = %v, want %v", c.dir, got, c.cc)
		}
	}
}

func names(es []Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

// ---- Sources: redirected config home ----

// A CLAUDE_CONFIG_DIR redirect must move skill discovery with it. `omac setup`
// already installs into ConfigHome()/skills, so a discovery layer still keyed
// on $HOME/.claude/skills resolves skills omac cannot grant into the sandbox.
func TestSources_ClaudeFollowsRedirectedConfigHome(t *testing.T) {
	home := withFakeHome(t)
	redirected := filepath.Join(home, ".work-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", redirected)
	stageSkill(t, filepath.Join(redirected, "skills"), "marketplace")

	wd := t.TempDir()
	dir, src, err := Resolve(wd, ccHarness(t), "marketplace")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join(redirected, "skills", "marketplace"); dir != want {
		t.Errorf("Resolve dir = %q, want %q", dir, want)
	}
	if src.Kind != "user-global" {
		t.Errorf("Resolve kind = %q, want user-global", src.Kind)
	}
}

// The superseded default root drops out entirely: resolving from ~/.claude
// would register a path ResolvedSandboxDirs no longer grants, so the sidecar
// could not be executed from it later.
func TestSources_RedirectDropsDefaultRoot(t *testing.T) {
	home := withFakeHome(t)
	redirected := filepath.Join(home, ".work-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", redirected)
	stageSkill(t, filepath.Join(redirected, "skills"), "marketplace")
	stageSkill(t, filepath.Join(home, ".claude", "skills"), "marketplace")

	wd := t.TempDir()
	for _, s := range Sources(wd, ccHarness(t)) {
		if s.Root == filepath.Join(home, ".claude", "skills") {
			t.Errorf("redirected claude scope must not include the default root; got %+v", s)
		}
	}
}

// A redirect SUBSTITUTES its root for the default one in place; it must not
// reorder the rest of the ladder. Prepending instead put the redirected root
// above $XDG_CONFIG_HOME/claude/skills, which the root it replaced ranked below.
func TestSources_RedirectPreservesRootPriority(t *testing.T) {
	home := withFakeHome(t)
	redirected := filepath.Join(home, ".work-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", redirected)

	// One skill of the same name in every user-global root, so Resolve's answer
	// is decided purely by candidate order.
	xdgRoot := filepath.Join(home, ".config", "claude", "skills")
	stageSkill(t, xdgRoot, "marketplace")
	stageSkill(t, filepath.Join(redirected, "skills"), "marketplace")

	dir, _, err := Resolve(t.TempDir(), ccHarness(t), "marketplace")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// ~/.claude/skills ranked BELOW ~/.config/claude/skills, so the root that
	// replaces it must too.
	if want := filepath.Join(xdgRoot, "marketplace"); dir != want {
		t.Errorf("Resolve dir = %q, want %q — the redirect must not outrank XDG roots", dir, want)
	}
}

// A CLAUDE_CONFIG_DIR that merely SPELLS the default home differently is not a
// redirect. Comparing raw strings made it one, and since the "redirected" root
// then equalled the root it superseded, the default skills root was dropped
// outright: every user-global claude skill became unresolvable.
func TestSources_TrailingSlashKeepsDefaultRoot(t *testing.T) {
	home := withFakeHome(t)
	stageSkill(t, filepath.Join(home, ".claude", "skills"), "marketplace")

	for _, spelling := range []string{
		filepath.Join(home, ".claude") + "/",
		filepath.Join(home, ".config", "..", ".claude"),
		"~/.claude",
	} {
		t.Run(spelling, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", spelling)
			dir, _, err := Resolve(t.TempDir(), ccHarness(t), "marketplace")
			if err != nil {
				t.Fatalf("skill in the default home became unresolvable: %v", err)
			}
			if want := filepath.Join(home, ".claude", "skills", "marketplace"); dir != want {
				t.Errorf("Resolve dir = %q, want %q", dir, want)
			}
		})
	}
}

// pi's config home is the nested ~/.pi/agent and codewhale owns the shared
// "agents" base, so neither harness's config-home skills dir was derivable from
// the skills bases — yet `omac setup` and launch-time provisioning install built-in
// skills into exactly those dirs, where discovery never looked.
func TestSources_ConfigHomeRootScannedForNestedHomes(t *testing.T) {
	for _, tc := range []struct {
		harness string
		rel     []string
	}{
		{"pi", []string{".pi", "agent", "skills"}},
		{"codewhale", []string{".codewhale", "skills"}},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			home := withFakeHome(t)
			h, ok := config.LookupHarness(tc.harness)
			if !ok {
				t.Fatalf("%s harness not registered", tc.harness)
			}
			root := filepath.Join(append([]string{home}, tc.rel...)...)
			if root != h.GlobalSkillsDir() {
				t.Fatalf("test stages %q but GlobalSkillsDir() is %q", root, h.GlobalSkillsDir())
			}
			stageSkill(t, root, "marketplace")

			dir, _, err := Resolve(t.TempDir(), h, "marketplace")
			if err != nil {
				t.Fatalf("skill in the harness's own global skills dir not found: %v", err)
			}
			if want := filepath.Join(root, "marketplace"); dir != want {
				t.Errorf("Resolve dir = %q, want %q", dir, want)
			}
		})
	}
}

// Without a redirect the candidate roots are unchanged — the default install
// keeps resolving out of ~/.claude/skills.
func TestSources_NoRedirectKeepsDefaultRoot(t *testing.T) {
	home := withFakeHome(t)
	stageSkill(t, filepath.Join(home, ".claude", "skills"), "marketplace")

	wd := t.TempDir()
	dir, _, err := Resolve(wd, ccHarness(t), "marketplace")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join(home, ".claude", "skills", "marketplace"); dir != want {
		t.Errorf("Resolve dir = %q, want %q", dir, want)
	}
}

// The redirect is per-harness: a Claude redirect must not perturb OpenCode
// discovery. OpenCode declares no HomeEnv at all, so its roots are derived from
// $XDG_CONFIG_HOME/$HOME alone.
func TestSources_OpenCodeUnaffectedByClaudeRedirect(t *testing.T) {
	home := withFakeHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".work-claude"))
	stageSkill(t, filepath.Join(home, ".config", "opencode", "skills"), "marketplace")

	wd := t.TempDir()
	dir, _, err := Resolve(wd, ocHarness(t), "marketplace")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := filepath.Join(home, ".config", "opencode", "skills", "marketplace"); dir != want {
		t.Errorf("Resolve dir = %q, want %q", dir, want)
	}
}
