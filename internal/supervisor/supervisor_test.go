package supervisor

import (
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
