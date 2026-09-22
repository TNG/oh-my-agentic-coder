package cli

import (
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/facade"
)

// promoteToReady promotes a pending-credentials skill to READY in both the
// dirState and the facade, then refreshes flat aliases. This lets the
// flat-alias security tests exercise the alias lifecycle without spawning
// real sidecars (which would need a supervisor and a working python3).
func promoteToReady(t *testing.T, s *serveServer, dir, mount, token string, port int) {
	t.Helper()
	s.mu.RLock()
	d := s.dirs[dir]
	s.mu.RUnlock()
	if d == nil {
		t.Fatalf("promoteToReady: dir %s not active", dir)
	}
	d.mu.Lock()
	sr := d.Skills[mount]
	if sr == nil {
		d.mu.Unlock()
		t.Fatalf("promoteToReady: skill %q not found in dir %s", mount, dir)
	}
	sr.State = facade.RouteReady
	d.mu.Unlock()
	s.facade.AddRoute(facade.Route{
		Mount:        mount,
		Namespace:    token,
		UpstreamPort: port,
		Skill:        sr.Name,
		Owner:        sr.Name,
		SkillDir:     sr.SkillDir,
		State:        facade.RouteReady,
	})
	s.refreshSingleDirAliases()
}

// stageManySkills stages count pending-credentials skills in dir, widening
// the activation window so a concurrent observer can sample the state
// between dir insertion and the trailing alias cleanup.
func stageManySkills(t *testing.T, dir string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		stageSkillWithSecret(t, dir, fmt.Sprintf("beta-%d", i))
	}
}

// TestSecurityFlatAliasesClearedBeforeSecondDirVisible pins the property
// that when a second directory activates, the first directory's flat
// (namespace "") aliases are already gone by the time the second dir is
// visible in s.dirs. The fix calls clearFlatAliases inside the same
// s.mu.Lock() section that inserts the new dirState; without it, the flat
// aliases survive until the trailing refreshSingleDirAliases at the end of
// activate(), leaving a window in which both dirs are visible while the
// first dir's token-less alias is still routeable.
func TestSecurityFlatAliasesClearedBeforeSecondDirVisible(t *testing.T) {
	s := newServeServerForTest(t)

	// Dir A: activate with a pending-credentials skill, then promote it to
	// READY so refreshSingleDirAliases installs the flat alias.
	dirA := t.TempDir()
	stageSkillWithSecret(t, dirA, "alpha")
	manifestA, err := s.activate(dirA)
	if err != nil {
		t.Fatalf("activate A: %v", err)
	}
	tokenA, _ := manifestA["dir_token"].(string)
	if tokenA == "" {
		t.Fatal("activate A returned no dir_token")
	}
	promoteToReady(t, s, dirA, "alpha", tokenA, 50001)
	if !s.facade.HasRoute("", "alpha") {
		t.Fatal("precondition: flat alias for A's ready skill should exist")
	}

	// Dir B: stage many skills to widen the activation window.
	dirB := t.TempDir()
	stageManySkills(t, dirB, 30)

	// Poll concurrently during activate(B). The invariant: at no sample
	// should len(s.dirs) >= 2 coexist with the flat alias still routeable.
	type obs struct {
		dirs int
		flat bool
	}
	var mu sync.Mutex
	var observations []obs
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.mu.RLock()
			n := len(s.dirs)
			s.mu.RUnlock()
			flat := s.facade.HasRoute("", "alpha")
			mu.Lock()
			observations = append(observations, obs{n, flat})
			mu.Unlock()
			runtime.Gosched()
		}
	}()

	if _, err := s.activate(dirB); err != nil {
		t.Fatalf("activate B: %v", err)
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	sawTwoDirs := false
	for _, o := range observations {
		if o.dirs >= 2 {
			sawTwoDirs = true
		}
		if o.dirs >= 2 && o.flat {
			t.Errorf("invariant violated: len(dirs)=%d and flat alias still routeable — "+
				"clearFlatAliases must run before the second dir is visible", o.dirs)
			break
		}
	}
	mu.Unlock()

	if !sawTwoDirs {
		t.Fatal("polling never observed two dirs; activation was too fast to sample the window")
	}

	// With two dirs active, the flat alias must be gone.
	if s.facade.HasRoute("", "alpha") {
		t.Error("post-activation: flat alias should be cleared with 2 dirs active")
	}
	// A's namespaced route must survive.
	if !s.facade.HasRoute(tokenA, "alpha") {
		t.Error("A's namespaced route should still exist")
	}
}

// TestSecurityFlatAliasGoneDuringSecondDirActivation is the race-style
// property: while B activates (slow, many staged skills), a concurrent
// observer must never see the first dir's flat alias routeable once B is
// visible. It also pins the deactivate transition: when B is later
// deactivated back to a single active directory, the flat aliases are
// correctly restored.
func TestSecurityFlatAliasGoneDuringSecondDirActivation(t *testing.T) {
	s := newServeServerForTest(t)

	dirA := t.TempDir()
	stageSkillWithSecret(t, dirA, "alpha")
	manifestA, err := s.activate(dirA)
	if err != nil {
		t.Fatalf("activate A: %v", err)
	}
	tokenA, _ := manifestA["dir_token"].(string)
	if tokenA == "" {
		t.Fatal("activate A returned no dir_token")
	}
	promoteToReady(t, s, dirA, "alpha", tokenA, 50002)
	if !s.facade.HasRoute("", "alpha") {
		t.Fatal("precondition: flat alias for A's ready skill should exist")
	}

	dirB := t.TempDir()
	stageManySkills(t, dirB, 30)

	// Concurrent observer: count how many times the flat alias is routeable
	// while two dirs are visible. Must be zero.
	var mu sync.Mutex
	var violations int
	var samples int
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.mu.RLock()
			n := len(s.dirs)
			s.mu.RUnlock()
			if n >= 2 {
				mu.Lock()
				samples++
				if s.facade.HasRoute("", "alpha") {
					violations++
				}
				mu.Unlock()
			}
			runtime.Gosched()
		}
	}()

	if _, err := s.activate(dirB); err != nil {
		t.Fatalf("activate B: %v", err)
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	v := violations
	sm := samples
	mu.Unlock()

	if sm == 0 {
		t.Fatal("polling never observed two dirs; activation was too fast to sample the window")
	}
	if v > 0 {
		t.Errorf("flat alias was routeable %d times while two dirs were visible — "+
			"the token-less alias must be cleared before the second dir becomes visible", v)
	}

	if s.facade.HasRoute("", "alpha") {
		t.Error("after activation: flat alias should be cleared with 2 dirs active")
	}

	// Deactivate B → back to a single dir. The flat alias must be restored.
	s.deactivate(dirB)

	if !s.facade.HasRoute("", "alpha") {
		t.Error("after deactivating B: flat alias should be restored for the single remaining dir")
	}
	if !s.facade.HasRoute(tokenA, "alpha") {
		t.Error("A's namespaced route should still exist after deactivating B")
	}
}
