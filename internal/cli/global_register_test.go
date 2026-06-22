package cli

import (
	"reflect"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
)

// TestMergeRegistries_WorkdirWins proves the union semantics: every
// name from both layers appears once, and the workdir entry shadows a
// same-named global entry.
func TestMergeRegistries_WorkdirWins(t *testing.T) {
	global := &registry.Registry{Registered: []registry.Entry{
		{Name: "shared", SkillDir: "/global/shared", BundleHash: "global"},
		{Name: "only-global", SkillDir: "/global/og"},
	}}
	workdir := &registry.Registry{Registered: []registry.Entry{
		{Name: "shared", SkillDir: ".opencode/skills/shared", BundleHash: "workdir"},
		{Name: "only-workdir", SkillDir: ".opencode/skills/ow"},
	}}

	merged := mergeRegistries(global, workdir)

	byName := map[string]registry.Entry{}
	for _, e := range merged.Registered {
		if _, dup := byName[e.Name]; dup {
			t.Fatalf("duplicate name in merged registry: %s", e.Name)
		}
		byName[e.Name] = e
	}
	if len(byName) != 3 {
		t.Fatalf("merged registry should have 3 unique skills, got %d: %+v", len(byName), merged.Registered)
	}
	if got := byName["shared"].BundleHash; got != "workdir" {
		t.Errorf("shared should resolve to the workdir entry, got bundle_hash=%q", got)
	}
	if _, ok := byName["only-global"]; !ok {
		t.Error("only-global should survive the merge")
	}
	if _, ok := byName["only-workdir"]; !ok {
		t.Error("only-workdir should survive the merge")
	}
}

// TestMergeRegistries_DoesNotMutateInputs guards against the merge
// helper aliasing or appending into either source slice.
func TestMergeRegistries_DoesNotMutateInputs(t *testing.T) {
	global := &registry.Registry{Registered: []registry.Entry{{Name: "g"}}}
	workdir := &registry.Registry{Registered: []registry.Entry{{Name: "w"}}}
	_ = mergeRegistries(global, workdir)
	if len(global.Registered) != 1 || len(workdir.Registered) != 1 {
		t.Fatalf("inputs mutated: global=%+v workdir=%+v", global.Registered, workdir.Registered)
	}
}

// TestMergeConfig_WorkdirWins checks the per-(skill,field) override:
// the workdir value beats the global one, but global-only fields and
// global-only skills still surface.
func TestMergeConfig_WorkdirWins(t *testing.T) {
	global := &skillconfig.Store{Skills: map[string]map[string]string{}}
	global.Set("email", "host", "global.example.com")
	global.Set("email", "port", "993")
	global.Set("calendar", "tz", "UTC")

	workdir := &skillconfig.Store{Skills: map[string]map[string]string{}}
	workdir.Set("email", "host", "workdir.example.com")

	merged := mergeConfig(global, workdir)

	if v, _ := merged.Get("email", "host"); v != "workdir.example.com" {
		t.Errorf("email/host = %q, want workdir override", v)
	}
	if v, _ := merged.Get("email", "port"); v != "993" {
		t.Errorf("email/port = %q, want global value 993", v)
	}
	if v, _ := merged.Get("calendar", "tz"); v != "UTC" {
		t.Errorf("calendar/tz = %q, want global-only value UTC", v)
	}

	// Inputs untouched.
	if v, _ := global.Get("email", "host"); v != "global.example.com" {
		t.Errorf("global store mutated: email/host = %q", v)
	}
	if fields := workdir.FieldsFor("email"); !reflect.DeepEqual(fields, []string{"host"}) {
		t.Errorf("workdir store mutated: email fields = %v", fields)
	}
}

// TestStartSeesGloballyRegisteredSkill is the end-to-end intent of the
// feature: a skill registered ONLY in the user-global registry must not
// be flagged as unregistered when starting from a fresh workdir, once
// the global layer is merged in — exactly what runStart now does.
func TestStartSeesGloballyRegisteredSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// A user-global skill source exists on disk...
	wd := t.TempDir()
	stageUserGlobalSkill(t, "tng-email")

	// ...and is registered ONLY in the global registry (never per
	// workdir).
	greg := &registry.Registry{Registered: []registry.Entry{{Name: "tng-email"}}}
	if err := registry.WithGlobalLock(func() error { return registry.SaveGlobal(greg) }); err != nil {
		t.Fatalf("SaveGlobal: %v", err)
	}

	// Merge the empty workdir registry with the global one, the way
	// runStart does, then verify nothing is reported unregistered.
	merged := mergeRegistries(greg, &registry.Registry{})
	_, unreg, err := findUnregisteredSkills(wd, config.DefaultHarness(), merged)
	if err != nil {
		t.Fatalf("findUnregisteredSkills: %v", err)
	}
	if len(unreg) != 0 {
		t.Fatalf("globally-registered skill should not be unregistered, got %v", unreg)
	}
}
