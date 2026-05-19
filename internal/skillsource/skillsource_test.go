package skillsource

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// withFakeHome installs a temporary HOME for the duration of the
// test, points XDG_CONFIG_HOME at $HOME/.config, and returns the
// home dir path. Any user-global skills the test creates should go
// under <home>/.config/opencode/skills or <home>/.opencode/skills.
//
// We use t.Setenv so cleanup is automatic on test exit; that's also
// the only way to safely mutate global env without racing other
// parallel tests in the same package.
func withFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Force a known XDG layout. Setting XDG_CONFIG_HOME explicitly
	// also exercises the early-return path in userGlobalRoots.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

// stageSkill drops a omac.yaml under <root>/<name>/ so Discover and
// Resolve count it as a skill. Body is intentionally minimal — we
// don't load or validate the meta in this package.
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

func TestSources_WorkdirAlwaysIncluded(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	got := Sources(wd)
	// Both workdir-local roots (.agents first, then .opencode) must
	// be present and come before any user-global root.
	if len(got) < 2 {
		t.Fatalf("expected at least the two workdir roots; got %+v", got)
	}
	wantFirst := filepath.Join(wd, ".agents", "skills")
	wantSecond := filepath.Join(wd, ".opencode", "skills")
	if got[0].Kind != "workdir" || got[0].Root != wantFirst {
		t.Errorf("source[0] = %+v, want kind=workdir root=%q", got[0], wantFirst)
	}
	if got[1].Kind != "workdir" || got[1].Root != wantSecond {
		t.Errorf("source[1] = %+v, want kind=workdir root=%q", got[1], wantSecond)
	}
}

func TestSources_UserGlobalAppearsWhenPresent(t *testing.T) {
	home := withFakeHome(t)
	// Create the XDG-style opencode root.
	root := filepath.Join(home, ".config", "opencode", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	got := Sources(t.TempDir())
	// First two entries are the two workdir-local roots; the
	// user-global root we just created should appear afterwards.
	if len(got) < 3 {
		t.Fatalf("expected at least 3 sources, got %+v", got)
	}
	var foundUG bool
	for _, s := range got {
		if s.Kind == "user-global" && s.Root == root {
			foundUG = true
			break
		}
	}
	if !foundUG {
		t.Errorf("user-global root %q not found in sources %+v", root, got)
	}
}

func TestSources_LegacyHomePath(t *testing.T) {
	home := withFakeHome(t)
	// Only stage the legacy ~/.opencode/skills, NOT the XDG one.
	root := filepath.Join(home, ".opencode", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	got := Sources(t.TempDir())
	var foundLegacy bool
	for _, s := range got {
		if s.Root == root {
			foundLegacy = true
			break
		}
	}
	if !foundLegacy {
		t.Errorf("legacy ~/.opencode/skills not picked up; got sources %+v", got)
	}
}

func TestSources_OmitsMissingUserGlobal(t *testing.T) {
	withFakeHome(t)
	// Don't create any user-global dir. Sources should only return
	// the workdir entry.
	got := Sources(t.TempDir())
	for _, s := range got {
		if s.Kind == "user-global" {
			t.Errorf("user-global source should not appear when no dir exists; got %+v", got)
		}
	}
}

func TestResolve_WorkdirWinsOverGlobal(t *testing.T) {
	home := withFakeHome(t)
	wd := t.TempDir()
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "echo-rest")
	stageSkill(t, filepath.Join(home, ".config", "opencode", "skills"), "echo-rest")

	dir, src, err := Resolve(wd, "echo-rest")
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

func TestResolve_FallsBackToGlobal(t *testing.T) {
	home := withFakeHome(t)
	wd := t.TempDir()
	stageSkill(t, filepath.Join(home, ".config", "opencode", "skills"), "tng-email")

	dir, src, err := Resolve(wd, "tng-email")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(home, ".config", "opencode", "skills", "tng-email")
	if dir != want {
		t.Errorf("Resolve picked %q, want %q", dir, want)
	}
	if src.Kind != "user-global" {
		t.Errorf("source.Kind = %q, want user-global", src.Kind)
	}
}

func TestResolve_NotFound(t *testing.T) {
	withFakeHome(t)
	_, _, err := Resolve(t.TempDir(), "nope")
	if err == nil {
		t.Fatal("expected error for missing skill")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err should be ErrNotExist, got %v", err)
	}
}

func TestDiscover_MergesAndDedupes(t *testing.T) {
	home := withFakeHome(t)
	wd := t.TempDir()

	// Workdir-only: alpha
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "alpha")

	// User-global only: bravo
	stageSkill(t, filepath.Join(home, ".config", "opencode", "skills"), "bravo")

	// Both layers: charlie. Workdir version should win (we'll detect
	// this via Kind=="workdir" on charlie's entry).
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "charlie")
	stageSkill(t, filepath.Join(home, ".config", "opencode", "skills"), "charlie")

	got, err := Discover(wd)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })

	want := []Entry{
		{Name: "alpha", Dir: filepath.Join(wd, ".opencode", "skills", "alpha"), Kind: "workdir"},
		{Name: "bravo", Dir: filepath.Join(home, ".config", "opencode", "skills", "bravo"), Kind: "user-global"},
		{Name: "charlie", Dir: filepath.Join(wd, ".opencode", "skills", "charlie"), Kind: "workdir"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Discover mismatch:\n got:  %+v\n want: %+v", got, want)
	}
}

func TestDiscover_SkipsDirsWithoutMeta(t *testing.T) {
	home := withFakeHome(t)
	wd := t.TempDir()
	// A bare directory without omac.yaml under skills/ is incidental
	// (e.g. _template/). It should NOT show up.
	if err := os.MkdirAll(filepath.Join(wd, ".opencode", "skills", "_template"), 0o755); err != nil {
		t.Fatal(err)
	}
	stageSkill(t, filepath.Join(home, ".config", "opencode", "skills"), "real")

	got, err := Discover(wd)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 || got[0].Name != "real" {
		t.Errorf("Discover = %+v, want exactly [real]", got)
	}
}

func TestDiscover_MissingDirsAreNoOp(t *testing.T) {
	withFakeHome(t)
	// Both layers are absent; Discover must return (nil, nil), not
	// an error.
	got, err := Discover(t.TempDir())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got != nil {
		t.Errorf("Discover with empty layers = %+v, want nil", got)
	}
}

func TestSources_AgentsRanksAboveOpencodeInWorkdir(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	got := Sources(wd)
	// Find the two workdir roots in the slice and assert .agents
	// comes first.
	var agentsIdx, opencodeIdx = -1, -1
	for i, s := range got {
		switch s.Root {
		case filepath.Join(wd, ".agents", "skills"):
			agentsIdx = i
		case filepath.Join(wd, ".opencode", "skills"):
			opencodeIdx = i
		}
	}
	if agentsIdx == -1 || opencodeIdx == -1 {
		t.Fatalf("both workdir roots must be present; got %+v", got)
	}
	if agentsIdx >= opencodeIdx {
		t.Errorf(".agents/skills (idx=%d) must rank above .opencode/skills (idx=%d); got %+v",
			agentsIdx, opencodeIdx, got)
	}
}

func TestResolve_AgentsWinsOverOpencodeInWorkdir(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	stageSkill(t, filepath.Join(wd, ".agents", "skills"), "echo-rest")
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "echo-rest")

	dir, src, err := Resolve(wd, "echo-rest")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(wd, ".agents", "skills", "echo-rest")
	if dir != want {
		t.Errorf("Resolve picked %q, want %q (.agents must win)", dir, want)
	}
	if src.Kind != "workdir" {
		t.Errorf("source.Kind = %q, want workdir", src.Kind)
	}
}

func TestResolve_FromAgentsOnly(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	stageSkill(t, filepath.Join(wd, ".agents", "skills"), "agents-only")

	dir, _, err := Resolve(wd, "agents-only")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(wd, ".agents", "skills", "agents-only")
	if dir != want {
		t.Errorf("Resolve from .agents/skills picked %q, want %q", dir, want)
	}
}

func TestResolve_UserGlobalAgentsRoot(t *testing.T) {
	home := withFakeHome(t)
	wd := t.TempDir()
	// Skill lives ONLY in the user-global agents root.
	stageSkill(t, filepath.Join(home, ".config", "agents", "skills"), "tng-email")

	dir, src, err := Resolve(wd, "tng-email")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(home, ".config", "agents", "skills", "tng-email")
	if dir != want {
		t.Errorf("Resolve picked %q, want %q", dir, want)
	}
	if src.Kind != "user-global" {
		t.Errorf("source.Kind = %q, want user-global", src.Kind)
	}
}

func TestResolve_UserGlobalLegacyAgents(t *testing.T) {
	home := withFakeHome(t)
	wd := t.TempDir()
	// Only the legacy ~/.agents/skills exists, not the XDG path.
	stageSkill(t, filepath.Join(home, ".agents", "skills"), "legacy-agents-skill")

	dir, src, err := Resolve(wd, "legacy-agents-skill")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(home, ".agents", "skills", "legacy-agents-skill")
	if dir != want {
		t.Errorf("Resolve picked %q, want %q", dir, want)
	}
	if src.Kind != "user-global" {
		t.Errorf("source.Kind = %q, want user-global", src.Kind)
	}
}

func TestDiscover_MergesAgentsAndOpencodeLayers(t *testing.T) {
	home := withFakeHome(t)
	wd := t.TempDir()

	// One skill in each of the four locations the production user
	// is most likely to have populated.
	stageSkill(t, filepath.Join(wd, ".agents", "skills"), "wd-agents-skill")
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "wd-opencode-skill")
	stageSkill(t, filepath.Join(home, ".config", "agents", "skills"), "ug-agents-skill")
	stageSkill(t, filepath.Join(home, ".config", "opencode", "skills"), "ug-opencode-skill")

	got, err := Discover(wd)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })

	want := []Entry{
		{Name: "ug-agents-skill", Dir: filepath.Join(home, ".config", "agents", "skills", "ug-agents-skill"), Kind: "user-global"},
		{Name: "ug-opencode-skill", Dir: filepath.Join(home, ".config", "opencode", "skills", "ug-opencode-skill"), Kind: "user-global"},
		{Name: "wd-agents-skill", Dir: filepath.Join(wd, ".agents", "skills", "wd-agents-skill"), Kind: "workdir"},
		{Name: "wd-opencode-skill", Dir: filepath.Join(wd, ".opencode", "skills", "wd-opencode-skill"), Kind: "workdir"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Discover mismatch:\n got:  %+v\n want: %+v", got, want)
	}
}

func TestDiscover_AgentsWinsOverOpencodeOnNameCollision(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()

	// Same skill name in both workdir-local layers.
	stageSkill(t, filepath.Join(wd, ".agents", "skills"), "shared")
	stageSkill(t, filepath.Join(wd, ".opencode", "skills"), "shared")

	got, err := Discover(wd)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 entry (deduped); got %+v", got)
	}
	want := filepath.Join(wd, ".agents", "skills", "shared")
	if got[0].Dir != want {
		t.Errorf("dedup picked %q, want %q (.agents must win)", got[0].Dir, want)
	}
}

func TestUserGlobalRoots_IncludesAgentsAndOpencode(t *testing.T) {
	home := withFakeHome(t)
	roots := userGlobalRoots()
	wantAny := []string{
		filepath.Join(home, ".config", "agents", "skills"),
		filepath.Join(home, ".config", "opencode", "skills"),
		filepath.Join(home, ".agents", "skills"),
		filepath.Join(home, ".opencode", "skills"),
	}
	for _, w := range wantAny {
		var found bool
		for _, r := range roots {
			if r == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("userGlobalRoots missing %q; got %+v", w, roots)
		}
	}
}

func TestUserGlobalRoots_DedupesIdenticalPaths(t *testing.T) {
	home := withFakeHome(t)
	// XDG_CONFIG_HOME explicitly set to $HOME/.config in withFakeHome,
	// which is also the default. The XDG-style path and the default
	// are the same string; Sources/userGlobalRoots must not list it
	// twice or we get duplicate scan work.
	roots := userGlobalRoots()
	seen := map[string]int{}
	for _, r := range roots {
		seen[r]++
	}
	for r, n := range seen {
		if n > 1 {
			t.Errorf("root %q listed %d times in userGlobalRoots; want 1", r, n)
		}
	}
	// Sanity: the XDG path under the fake home must appear.
	want := filepath.Join(home, ".config", "opencode", "skills")
	if seen[want] == 0 {
		t.Errorf("expected %q in roots; got %v", want, roots)
	}
}
