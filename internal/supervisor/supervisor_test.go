package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// envMap turns buildEnv's []string ("K=V") into a map for assertions.
func envMap(kv []string) map[string]string {
	m := make(map[string]string, len(kv))
	for _, e := range kv {
		if i := strings.IndexByte(e, '='); i >= 0 {
			m[e[:i]] = e[i+1:]
		}
	}
	return m
}

func TestBuildEnvSidecarSkillIsPlainName(t *testing.T) {
	s := New(nil)

	// SkillName set (serve mode): SIDECAR_SKILL must be the plain name,
	// never the namespaced tracking Name (which contains a slash that
	// breaks sidecar filesystem-path construction).
	env := envMap(s.buildEnv(SidecarSpec{
		Name:      "__global__/skill-marketplace",
		SkillName: "skill-marketplace",
		Workdir:   "/proj",
	}, 1234))
	if got := env["SIDECAR_SKILL"]; got != "skill-marketplace" {
		t.Errorf("SIDECAR_SKILL = %q, want skill-marketplace", got)
	}
	if strings.Contains(env["SIDECAR_SKILL"], "/") {
		t.Errorf("SIDECAR_SKILL must not contain '/': %q", env["SIDECAR_SKILL"])
	}
	if env["OMAC_WORKDIR"] != "/proj" {
		t.Errorf("OMAC_WORKDIR = %q, want /proj", env["OMAC_WORKDIR"])
	}

	// SkillName empty (start mode): falls back to Name.
	env2 := envMap(s.buildEnv(SidecarSpec{Name: "slack"}, 1))
	if env2["SIDECAR_SKILL"] != "slack" {
		t.Errorf("fallback SIDECAR_SKILL = %q, want slack", env2["SIDECAR_SKILL"])
	}
}

// TestStopSidecarTracking verifies the bookkeeping of StopSidecar without
// spawning real processes: a Running with a nil Cmd.Process terminates as a
// no-op (terminate handles nil), so we can assert set membership directly.
func TestStopSidecarTracking(t *testing.T) {
	s := New(nil)
	s.children = []*Running{
		{Name: "a"},
		{Name: "b"},
		{Name: "c"},
	}

	if ok := s.StopSidecar("b", time.Second); !ok {
		t.Fatal("StopSidecar(b) returned false, want true")
	}
	if len(s.children) != 2 {
		t.Fatalf("after stop, len = %d, want 2", len(s.children))
	}
	for _, r := range s.children {
		if r.Name == "b" {
			t.Fatal("b still tracked after StopSidecar")
		}
	}

	// Stopping an unknown name is a no-op.
	if ok := s.StopSidecar("zzz", time.Second); ok {
		t.Fatal("StopSidecar(zzz) returned true, want false")
	}
	if len(s.children) != 2 {
		t.Fatalf("len changed on no-op stop: %d", len(s.children))
	}

	// Remaining order preserved (a, c).
	if s.children[0].Name != "a" || s.children[1].Name != "c" {
		t.Fatalf("order not preserved: %s, %s", s.children[0].Name, s.children[1].Name)
	}
}

func TestEnsureExecutable(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "scripts", "sidecar.py")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/usr/bin/env python3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Not executable yet.
	if fi, _ := os.Stat(script); fi.Mode()&0o100 != 0 {
		t.Fatal("precondition: script should not be executable")
	}
	ensureExecutable(dir, "./scripts/sidecar.py")
	fi, _ := os.Stat(script)
	if fi.Mode()&0o100 == 0 {
		t.Error("ensureExecutable did not set the execute bit")
	}

	// Bare interpreter name: no-op (must not panic / touch anything).
	ensureExecutable(dir, "python3")

	// Path escaping the skill dir: ignored.
	outside := filepath.Join(t.TempDir(), "evil.sh")
	os.WriteFile(outside, []byte("x"), 0o644)
	ensureExecutable(dir, outside)
	if fi, _ := os.Stat(outside); fi.Mode()&0o100 != 0 {
		t.Error("ensureExecutable touched a file outside the skill dir")
	}
}
