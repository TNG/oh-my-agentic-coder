package cli

// Issue #514: a spawn refusal must name which registered copy of a skill it
// means and hand the user the `omac register` command that will actually run,
// instead of a bare name that `omac register` then refuses as ambiguous.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skillstate"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
)

func TestSkillCopyRegisterCmd(t *testing.T) {
	workdir := t.TempDir()
	cases := []struct {
		name  string
		entry registry.Entry
		want  string
	}{
		{"shared base, no harness recorded",
			registry.Entry{Name: "omac-gh", SkillDir: ".agents/skills/omac-gh"},
			"omac register omac-gh"},
		{"workdir skill scoped to a harness",
			registry.Entry{Name: "echo-rest", Harness: "opencode", SkillDir: ".opencode/skills/echo-rest"},
			"omac register echo-rest --harness opencode"},
		{"user-global skill",
			registry.Entry{Name: "skill-marketplace", Harness: "claude-code", SkillDir: filepath.Join(t.TempDir(), "skills", "skill-marketplace")},
			"omac register skill-marketplace --harness claude-code --global"},
		{"absolute path inside the workdir is not global",
			registry.Entry{Name: "local", Harness: "opencode", SkillDir: filepath.Join(workdir, ".opencode", "skills", "local")},
			"omac register local --harness opencode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			absDir := c.entry.SkillDir
			if !filepath.IsAbs(absDir) {
				absDir = filepath.Join(workdir, absDir)
			}
			got := skillCopyOf(c.entry, absDir, workdir).registerCmd()
			if got != c.want {
				t.Errorf("registerCmd = %q, want %q", got, c.want)
			}
		})
	}
}

// stageLegacySnapshot approves src for name and then moves its snapshot to the
// retired <store>/skills root, reproducing a store written before v0.13.1.
func stageLegacySnapshot(t *testing.T, name, src string) string {
	t.Helper()
	h := bundleHashOf(t, src)
	if err := skilltrust.Approve(name, h, src); err != nil {
		t.Fatal(err)
	}
	snap, ok := skilltrust.SnapshotPath(name, h)
	if !ok {
		t.Fatal("snapshot missing right after Approve")
	}
	legacy := filepath.Join(registry.GlobalDir(), "skills", name, strings.TrimPrefix(h, "sha256:"))
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(snap, legacy); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestApprovedSpawnDirForClassifiesRefusals(t *testing.T) {
	isolateHome(t)
	workdir := stageWorkdir(t, "fresh", "stale", "gone")
	entry := func(name string) (registry.Entry, string) {
		e := registry.Entry{Name: name, Harness: "opencode", SkillDir: filepath.Join(".opencode", "skills", name)}
		return e, filepath.Join(workdir, e.SkillDir)
	}

	// Never registered from the host: no approval at all.
	e, abs := entry("fresh")
	_, err := approvedSpawnDirFor(skillCopyOf(e, abs, workdir), "")
	var r *spawnRefusal
	if !errors.As(err, &r) || r.Kind != skillstate.NotApproved {
		t.Fatalf("fresh: err = %v, want a NotApproved spawnRefusal", err)
	}
	for _, want := range []string{`"fresh"`, abs, "not host-approved", "`omac register fresh --harness opencode`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("fresh refusal missing %q: %v", want, err)
		}
	}
	p := r.Problem()
	if p.Kind != skillstate.NotApproved || p.Skill != "fresh" || p.Fix != "omac register fresh --harness opencode" {
		t.Errorf("fresh problem = %+v", p)
	}

	// Approved before the store format changed: only the legacy snapshot exists.
	e, abs = entry("stale")
	h := stageLegacySnapshot(t, "stale", abs)
	_, err = approvedSpawnDirFor(skillCopyOf(e, abs, workdir), h)
	if !errors.As(err, &r) || r.Kind != skillstate.SnapshotStale {
		t.Fatalf("stale: err = %v, want a SnapshotStale spawnRefusal", err)
	}
	for _, want := range []string{"older copy policy", skilltrust.SnapshotPolicySince, "`omac register stale --harness opencode`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("stale refusal missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "not host-approved") {
		t.Errorf("approved skill reported as unapproved: %v", err)
	}

	// Approved, but the snapshot was deleted by hand: nothing under either root.
	e, abs = entry("gone")
	h = bundleHashOf(t, abs)
	if err := skilltrust.Approve("gone", h, abs); err != nil {
		t.Fatal(err)
	}
	snap, _ := skilltrust.SnapshotPath("gone", h)
	if err := os.RemoveAll(snap); err != nil {
		t.Fatal(err)
	}
	_, err = approvedSpawnDirFor(skillCopyOf(e, abs, workdir), h)
	if !errors.As(err, &r) || r.Kind != skillstate.SnapshotMissing {
		t.Fatalf("gone: err = %v, want a SnapshotMissing spawnRefusal", err)
	}
	if !strings.Contains(err.Error(), "snapshot is missing") {
		t.Errorf("gone refusal should say the snapshot is missing: %v", err)
	}
}

// TestApprovedSpawnDirNameOnlyKeepsBareRemedy: the name-only wrapper (used by
// the supervisor backstop, which has no registry entry) still hands out the
// plain command, so nothing it prints regresses.
func TestApprovedSpawnDirNameOnlyKeepsBareRemedy(t *testing.T) {
	isolateHome(t)
	workdir := stageWorkdir(t, "bare")
	_, err := approvedSpawnDir("bare", filepath.Join(workdir, ".opencode", "skills", "bare"), "")
	if err == nil || !strings.Contains(err.Error(), "`omac register bare`") {
		t.Errorf("err = %v, want the bare register command", err)
	}
}

func TestRefusalProblemWrapsUntypedErrors(t *testing.T) {
	p := refusalProblem("odd", errors.New("cannot read the host approval store: boom"))
	if p.Kind != skillstate.SpawnRefused || p.Skill != "odd" || !strings.Contains(p.Detail, "boom") || p.Fix != "" {
		t.Errorf("problem = %+v", p)
	}
	typed := &spawnRefusal{Kind: skillstate.NotApproved, Skill: skillCopy{Name: "typed"}}
	if got := refusalProblem("typed", typed); got.Kind != skillstate.NotApproved || got.Fix != "omac register typed" {
		t.Errorf("typed problem = %+v", got)
	}
}
