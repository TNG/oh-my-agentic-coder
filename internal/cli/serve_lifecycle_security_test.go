package cli

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skillconfig"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
	"github.com/TNG/oh-my-agentic-coder/internal/supervisor"
)

func TestServeFailedAdmissionLeavesCapacity(t *testing.T) {
	for _, failure := range []string{"discovery", "registry", "config"} {
		t.Run(failure, func(t *testing.T) {
			s := newServeServerForTest(t)
			s.maxDirs = 1
			bad, good := t.TempDir(), t.TempDir()
			path := registry.Path(bad)
			switch failure {
			case "discovery":
				path = filepath.Join(bad, ".opencode", "skills")
			case "config":
				path = skillconfig.Path(bad)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.activate(bad); err == nil {
				t.Fatal("invalid prerequisite accepted")
			}
			if len(s.dirs) != 0 || len(s.byToken) != 0 || len(s.tokenMemory) != 0 {
				t.Error("failed admission published directory/token state")
			}
			if _, err := s.activate(good); err != nil {
				t.Fatalf("valid admission after failure: %v", err)
			}
		})
	}
}

func TestServeConcurrentCapacityAndLifetime(t *testing.T) {
	s := newServeServerForTest(t)
	s.maxDirs = 4
	base := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		dir := filepath.Join(base, fmt.Sprint(i))
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.activate(dir) }()
	}
	wg.Wait()
	if len(s.dirs) != 4 || len(s.byToken) != 4 {
		t.Fatalf("dirs=%d tokens=%d", len(s.dirs), len(s.byToken))
	}
	var dir, token string
	for key, d := range s.dirs {
		dir, token = key, d.Token
		break
	}
	if _, err := s.activate(dir); err != nil {
		t.Fatal("existing directory rejected at cap:", err)
	}
	s.deactivate(dir)
	back, err := s.activate(dir)
	if err != nil {
		t.Fatal("capacity not reusable:", err)
	}
	if _, ok := back["dir_token"]; ok {
		t.Error("lifetime token re-emitted")
	}
	if s.dirs[dir].Token != token {
		t.Error("lifetime token rotated")
	}
}

func TestServeFailedAdmissionPreservesLiveAliases(t *testing.T) {
	requireWorkingPython3(t)
	s := newServeServerForTest(t)
	s.maxDirs = 2
	wd, bad := t.TempDir(), t.TempDir()
	dir, hash := stageAgentAuthoredSkill(t, wd, "demo", filepath.Join(t.TempDir(), "unused"))
	if err := skilltrust.Approve("demo", hash, dir); err != nil {
		t.Fatal(err)
	}
	s.sup = supervisor.New([]string{"PATH", "HOME", "LANG", "LC_ALL"}, audit.Nop(), skillSpawnAuthorizer)
	t.Cleanup(func() { s.sup.ShutdownAll(time.Second) })
	first, err := s.activate(wd)
	if err != nil {
		t.Fatal(err)
	}
	token := first["dir_token"].(string)
	if !s.facade.HasRoute("", "demo") {
		t.Fatal("initial flat alias absent")
	}
	if err := os.MkdirAll(filepath.Dir(registry.Path(bad)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registry.Path(bad), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.activate(bad); err == nil {
		t.Fatal("malformed registry accepted")
	}
	if len(s.dirs) != 1 || len(s.byToken) != 1 || !s.facade.HasRoute(token, "demo") || !s.facade.HasRoute("", "demo") {
		t.Fatal("failed admission changed live state/routes/aliases")
	}
}

func TestServeDeletionWaitsForInitialEmission(t *testing.T) {
	s := newServeServerForTest(t)
	wd := t.TempDir()
	dir, hash := stageAgentAuthoredSkill(t, wd, "demo", filepath.Join(t.TempDir(), "unused"))
	if err := skilltrust.Approve("demo", hash, dir); err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.sup = supervisor.New([]string{"PATH", "HOME"}, audit.Nop(), func(spec supervisor.SidecarSpec) error {
		if err := skillSpawnAuthorizer(spec); err != nil {
			return err
		}
		once.Do(func() { close(entered); <-resume })
		return errors.New("test: refuse exec after slow authorization")
	})
	t.Cleanup(func() { s.sup.ShutdownAll(time.Second) })
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	t.Cleanup(release)
	type result struct {
		m   map[string]any
		err error
	}
	activated := make(chan result, 1)
	go func() { m, err := s.activate(wd); activated <- result{m, err} }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no authorization barrier")
	}
	deleted := make(chan struct{})
	go func() { s.deactivate(wd); close(deleted) }()
	select {
	case <-deleted:
		t.Error("deactivation overtook unfinished initial activation/emission")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	first := <-activated
	if first.err != nil {
		t.Fatal(first.err)
	}
	if token, _ := first.m["dir_token"].(string); token == "" {
		t.Fatal("first activation did not emit")
	}
	select {
	case <-deleted:
	case <-time.After(5 * time.Second):
		t.Fatal("deactivation did not finish")
	}
	back, err := s.activate(wd)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := back["dir_token"]; ok {
		t.Error("overlap re-emitted lifetime token")
	}
}

type blockingLifecycleExitAuditor struct {
	audit.Auditor
	once            sync.Once
	entered, resume chan struct{}
}

func (a *blockingLifecycleExitAuditor) Emit(ev audit.Event) {
	if ev.Type == audit.TypeProcessExit {
		a.once.Do(func() { close(a.entered); <-a.resume })
	}
}

func TestServeReactivationWaitsForCompleteTeardown(t *testing.T) {
	requireWorkingPython3(t)
	s := newServeServerForTest(t)
	wd := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		dir, hash := stageAgentAuthoredSkill(t, wd, name, filepath.Join(t.TempDir(), "unused"))
		if err := skilltrust.Approve(name, hash, dir); err != nil {
			t.Fatal(err)
		}
	}
	a := &blockingLifecycleExitAuditor{Auditor: audit.Nop(), entered: make(chan struct{}), resume: make(chan struct{})}
	s.sup = supervisor.New([]string{"PATH", "HOME", "LANG", "LC_ALL"}, a, skillSpawnAuthorizer)
	t.Cleanup(func() { s.sup.ShutdownAll(time.Second) })
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(a.resume) }) }
	t.Cleanup(release)
	first, err := s.activate(wd)
	if err != nil {
		t.Fatal(err)
	}
	token := first["dir_token"].(string)
	for _, name := range []string{"alpha", "beta"} {
		if s.dirs[wd].Skills[name].State != facade.RouteReady {
			t.Fatal("initial skill not ready:", name)
		}
	}
	deleted := make(chan struct{})
	go func() { s.deactivate(wd); close(deleted) }()
	select {
	case <-a.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no process-exit barrier")
	}
	type result struct {
		m   map[string]any
		err error
	}
	reopened := make(chan result, 1)
	go func() { m, err := s.activate(wd); reopened <- result{m, err} }()
	var back result
	overtook := false
	select {
	case back = <-reopened:
		overtook = true
		if back.err != nil {
			t.Fatal(back.err)
		}
		// This is the original R1 counterexample: new routes really exist
		// before the old teardown resumes, not just a stale map entry.
		for _, name := range []string{"alpha", "beta"} {
			if !s.facade.HasRoute(token, name) {
				t.Fatal("new route not installed:", name)
			}
		}
		t.Error("reactivation overtook incomplete teardown")
	case <-time.After(2 * time.Second):
	}
	release()
	select {
	case <-deleted:
	case <-time.After(5 * time.Second):
		t.Fatal("deactivation did not finish")
	}
	if !overtook {
		select {
		case back = <-reopened:
		case <-time.After(5 * time.Second):
			t.Fatal("reactivation did not finish")
		}
	}
	if back.err != nil {
		t.Fatal(back.err)
	}
	if _, ok := back.m["dir_token"]; ok {
		t.Error("reopen re-emitted token")
	}
	for _, name := range []string{"alpha", "beta"} {
		if !s.facade.HasRoute(token, name) {
			t.Errorf("old teardown removed new route %s", name)
		}
	}
}

func TestServeLiveBridgeReopen(t *testing.T) {
	requireWorkingPython3(t)
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node required for bridge integration")
	}
	s := newServeServerForTest(t)
	wd := t.TempDir()
	dir, hash := stageAgentAuthoredSkill(t, wd, "demo", filepath.Join(t.TempDir(), "unused"))
	if err := skilltrust.Approve("demo", hash, dir); err != nil {
		t.Fatal(err)
	}
	s.sup = supervisor.New([]string{"PATH", "HOME", "LANG", "LC_ALL"}, audit.Nop(), skillSpawnAuthorizer)
	t.Cleanup(func() { s.sup.ShutdownAll(time.Second) })
	server := httptest.NewServer(s.controlMux())
	t.Cleanup(server.Close)
	cmd := exec.Command("node", "../plugin/test/live-reopen.mjs")
	cmd.Env = append(os.Environ(), "OMAC_CONTROL_BASE="+server.URL, "OMAC_CONTROL_TOKEN="+s.controlToken, "OMAC_TEST_PROJECT="+wd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("live server + bridge: %v\n%s", err, out)
	}
}
