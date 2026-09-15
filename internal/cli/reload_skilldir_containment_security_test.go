//go:build vuln

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// TestSecurityReloadDoesNotWalkArbitraryHostPaths asserts that a registry
// entry naming an absolute SkillDir with no relation to the workdir does
// not get its content read/hashed on the host before being refused.
//
// start_reload.go absolutizes SkillDir only when it is relative
// (filepath.Join(workdir, absDir) — reached only when NOT already
// absolute), so an already-absolute forged entry passes through
// unvalidated. It is then handed straight to resolver.Inspect and
// approvedSpawnDir, both of which call config.BundleHash on it — walking
// and reading every file in that tree — before any approval check runs.
// This proves the walk reaches an unrelated host directory by making one
// of its subdirectories unreadable: if the host never touched it, the walk
// (and thus the whole reload) succeeds cleanly; instead the resulting
// refusal message reports a permission error naming that unrelated path.
func TestSecurityReloadDoesNotWalkArbitraryHostPaths(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	isolateHome(t)
	workdir := t.TempDir()

	foreign := t.TempDir() // has nothing to do with workdir
	if err := os.WriteFile(filepath.Join(foreign, config.MetaFileName), []byte(
		"name: forged\ntype: skill\nsidecar:\n  command: [\"true\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(foreign, "unreadable")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) }) // let TempDir cleanup succeed

	// Control: BundleHash on the foreign tree does in fact fail because of
	// the blocked subdirectory, naming it in the error — proves the
	// fixture actually reaches the walk this test is about, independent of
	// the reload path.
	if _, err := config.BundleHash(foreign); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Skipf("control: BundleHash did not fail on the planted unreadable subdir as expected (err=%v): likely running as a uid that ignores permission bits (e.g. root)", err)
	}

	if err := registry.WithLock(workdir, func() error {
		reg, err := registry.Load(workdir)
		if err != nil {
			return err
		}
		reg.Upsert(registry.Entry{
			Name:         "forged",
			SkillDir:     foreign, // already absolute — passes through unvalidated
			BundleHash:   "",
			RegisteredAt: time.Now().UTC(),
		})
		return registry.Save(workdir, reg)
	}); err != nil {
		t.Fatalf("forge registry: %v", err)
	}

	r, baseURL := newLiveReloader(t, workdir)
	r.reload()

	if r.isMounted("forged") {
		t.Fatal("control: an unapproved forged skill was mounted — the approval gate itself is broken, unrelated to this test")
	}

	_, body := httpGet(t, baseURL+"/forged/status")
	if strings.Contains(body, blocked) || strings.Contains(body, "unreadable") {
		t.Errorf("reload's refusal message for a registry entry naming an absolute SkillDir with no relation to the workdir reports a permission error from inside that foreign directory (%q), proving the host walked and read it before any containment check: %q", foreign, body)
	}
}
