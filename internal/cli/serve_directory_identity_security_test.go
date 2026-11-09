package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
)

func waitActivationStack(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(stack, "(*serveServer).activate(") && strings.Contains(stack, marker) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("activation did not block at %s", marker)
		}
		runtime.Gosched()
	}
}

// TestSecurityServeSwapAfterValidationRefused pauses activation at the
// per-directory lock, after admission but before discovery and registration,
// and replaces the validated name with a link to a foreign tree. Activation
// must refuse instead of working on the replacement.
func TestSecurityServeSwapAfterValidationRefused(t *testing.T) {
	s := newServeServerForTest(t)
	root := resolvedTempDir(t)
	outside := resolvedTempDir(t)
	s.roots = []string{root}
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	stageSkillWithSecret(t, outside, "foreign")
	lock := s.dirActLock(project)
	lock.Lock()
	held := true
	defer func() {
		if held {
			lock.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() { _, err := s.activate(project); done <- err }()
	waitActivationStack(t, "Mutex")
	if err := os.Rename(project, project+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, project); err != nil {
		t.Fatal(err)
	}
	lock.Unlock()
	held = false
	if err := <-done; err == nil {
		t.Error("activation succeeded after validated directory was replaced by an outside symlink")
	}
	if _, err := os.Stat(registry.Path(outside)); !os.IsNotExist(err) {
		t.Errorf("host registration wrote outside the root: %v", err)
	}
	assertActiveDirs(t, s)
}

func TestSecurityServeWaitingRediscoveryReplacement(t *testing.T) {
	for _, ancestor := range []bool{false, true} {
		t.Run(fmt.Sprintf("ancestor=%v", ancestor), func(t *testing.T) {
			s := newServeServerForTest(t)
			root, outside := resolvedTempDir(t), resolvedTempDir(t)
			s.roots = []string{root}
			parent := filepath.Join(root, "parent")
			project := filepath.Join(parent, "project")
			stageSkillWithSecret(t, project, "resident")
			m, err := s.activate(project)
			if err != nil {
				t.Fatal(err)
			}
			token := m["dir_token"].(string)
			foreign := outside
			if ancestor {
				foreign = filepath.Join(outside, "project")
			}
			stageSkillWithSecret(t, foreign, "foreign")
			lock := s.dirActLock(project)
			lock.Lock()
			held := true
			defer func() {
				if held {
					lock.Unlock()
				}
			}()
			done := make(chan error, 1)
			go func() { _, err := s.activate(project); done <- err }()
			waitActivationStack(t, "Mutex")
			swap := project
			if ancestor {
				swap = parent
			}
			if err := os.Rename(swap, swap+"-saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, swap); err != nil {
				t.Fatal(err)
			}
			lock.Unlock()
			held = false
			if err := <-done; err == nil {
				t.Error("already-active replacement was admitted")
			}
			if _, err := os.Stat(registry.Path(foreign)); !os.IsNotExist(err) {
				t.Errorf("rediscovery wrote outside: %v", err)
			}
			if len(s.dirs) != 1 || s.byToken[token] == nil || !s.facade.HasRoute(token, "resident") || s.facade.HasRoute(token, "foreign") {
				t.Fatal("rediscovery changed managed identity")
			}
		})
	}
}

// This mutation happens AFTER the admission and activation-lock checks. An
// extra pathname revalidation cannot pass it: the real registry flock is the
// scheduling barrier, and the eventual load/save must use the opened identity.
func TestSecurityServeLateReplacementRegistryIdentity(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, ancestor := range []bool{false, true} {
			t.Run(fmt.Sprintf("active=%v/ancestor=%v", active, ancestor), func(t *testing.T) {
				s := newServeServerForTest(t)
				root, outside := resolvedTempDir(t), resolvedTempDir(t)
				s.roots = []string{root}
				parent := filepath.Join(root, "parent")
				project := filepath.Join(parent, "project")
				if err := os.MkdirAll(project, 0o755); err != nil {
					t.Fatal(err)
				}
				var token string
				if active {
					m, err := s.activate(project)
					if err != nil {
						t.Fatal(err)
					}
					token = m["dir_token"].(string)
				}
				stageSkillWithSecret(t, project, "resident")
				outsideProject := outside
				if ancestor {
					outsideProject = filepath.Join(outside, "project")
				}
				stageSkillWithSecret(t, outsideProject, "foreign")
				f, err := os.OpenFile(registry.LockPath(project), os.O_CREATE|os.O_RDWR, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
					t.Fatal(err)
				}
				defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				done := make(chan error, 1)
				go func() { _, err := s.activate(project); done <- err }()
				waitActivationStack(t, "syscall.Flock")
				swap := project
				if ancestor {
					swap = parent
				}
				if err := os.Rename(swap, swap+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, swap); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatalf("pinned activation: %v", err)
				}
				if _, err := os.Stat(registry.Path(outsideProject)); !os.IsNotExist(err) {
					t.Fatalf("outside registry touched: %v", err)
				}
				saved := project + "-saved"
				if ancestor {
					saved = filepath.Join(parent+"-saved", "project")
				}
				reg, err := registry.Load(saved)
				if err != nil {
					t.Fatal(err)
				}
				e, _ := reg.Find("resident")
				if e == nil || e.BundleHash == "" {
					t.Fatal("legitimate registration was lost")
				}
				if e, _ := reg.Find("foreign"); e != nil {
					t.Fatal("registered outside skill")
				}
				d := s.dirs[project]
				if d == nil {
					t.Fatal("original directory state missing")
				}
				if active && d.Token != token {
					t.Fatal("rediscovery replaced original token")
				}
				if !s.facade.HasRoute(d.Token, "resident") || s.facade.HasRoute(d.Token, "foreign") {
					t.Fatal("wrong routed identity")
				}
				if d.Skills["resident"].State != facade.RoutePendingCredentials {
					t.Fatalf("positive host-approval control: %+v", d.Skills["resident"])
				}
				// The final-component rule still rejects replacement on the next turn.
				if _, err := s.activate(project); err == nil {
					t.Fatal("replacement admitted on next activation")
				}
			})
		}
	}
}

func TestSecurityServeAliasTeardownIdentity(t *testing.T) {
	s := newServeServerForTest(t)
	root := resolvedTempDir(t)
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	stageSkillWithSecret(t, filepath.Join(a, "project"), "resident")
	stageSkillWithSecret(t, filepath.Join(b, "project"), "other")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(a, alias); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(alias, "project")
	m, err := s.activate(name)
	if err != nil {
		t.Fatal(err)
	}
	token := m["dir_token"].(string)
	m, err = s.activate(filepath.Join(b, "project"))
	if err != nil {
		t.Fatal(err)
	}
	other := m["dir_token"].(string)
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(b, alias); err != nil {
		t.Fatal(err)
	}
	w := controlRequest(s, "deactivate", name)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("retarget status=%d body=%s", w.Code, w.Body)
	}
	if !s.facade.HasRoute(token, "resident") || !s.facade.HasRoute(other, "other") || len(s.dirs) != 2 {
		t.Fatal("retarget tore down a directory")
	}
	if err := os.RemoveAll(filepath.Join(a, "project")); err != nil {
		t.Fatal(err)
	}
	w = controlRequest(s, "deactivate", filepath.Join(a, "project"))
	if w.Code != http.StatusOK {
		t.Fatalf("deleted canonical teardown: %d %s", w.Code, w.Body)
	}
	if s.byToken[token] != nil || s.facade.HasRoute(token, "resident") {
		t.Fatal("deleted identity retained token/route")
	}
	for n, d := range s.dirNames {
		if d.Token == token {
			t.Fatalf("stale alias mapping: %s", n)
		}
	}
	if s.byToken[other] == nil || !s.facade.HasRoute(other, "other") {
		t.Fatal("other directory was torn down")
	}
	w = controlRequest(s, "deactivate", filepath.Join(root, "never-admitted"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown missing name falsely succeeded: %d", w.Code)
	}
}

func controlRequest(s *serveServer, action, name string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/__omac__/"+action, strings.NewReader(fmt.Sprintf(`{"dir":%q}`, name)))
	addServeTestToken(req)
	w := httptest.NewRecorder()
	s.controlMux().ServeHTTP(w, req)
	return w
}

// An escaping .opencode replacement must not redirect the registry open/save
// even after discovery and hashing have completed on the legitimate skill.
func TestSecurityServeLateRegistryAncestorEscape(t *testing.T) {
	s := newServeServerForTest(t)
	root, outside := resolvedTempDir(t), resolvedTempDir(t)
	s.roots = []string{root}
	project := filepath.Join(root, "project")
	stageSkillWithSecret(t, project, "resident")
	if err := os.Mkdir(filepath.Join(outside, ".opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(registry.LockPath(project), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	done := make(chan error, 1)
	go func() { _, err := s.activate(project); done <- err }()
	waitActivationStack(t, "syscall.Flock")
	path := filepath.Join(project, ".opencode")
	if err := os.Rename(path, path+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, ".opencode"), path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(registry.Path(outside)); !os.IsNotExist(err) {
		t.Fatalf("outside write: %v", err)
	}
	d := s.dirs[project]
	if d == nil || d.State != "active_partial" || d.Skills["resident"].State != facade.RouteBroken {
		t.Fatal("escaping registry did not fail closed")
	}
	if _, err := s.activate(project); err == nil {
		t.Fatal("rediscovery silently accepted escaping registry ancestor")
	}
}
