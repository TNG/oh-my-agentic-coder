package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func reviewPhysicalTemp(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Pause at the existing per-directory mutex, after the new validation but
// before discovery/registration, without modifying any production source.
func TestReviewR1SwapAfterValidation(t *testing.T) {
	s := newServeServerForTest(t)
	root := reviewPhysicalTemp(t)
	outside := reviewPhysicalTemp(t)
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		blocked := false
		for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(stack, "(*serveServer).activate(") && strings.Contains(stack, "sync") && strings.Contains(stack, "Mutex") {
				blocked = true
				break
			}
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("activation did not reach held per-dir lock")
		}
		runtime.Gosched()
	}
	if err := os.Rename(project, project+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, project); err != nil {
		t.Fatal(err)
	}
	lock.Unlock()
	held = false
	err := <-done
	if err == nil {
		t.Error("activation succeeded after validated directory was replaced by an outside symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, ".opencode", "sidecar.json")); err == nil {
		t.Error("host registration wrote .opencode/sidecar.json in outside-root target")
	}
	for p, d := range s.dirs {
		t.Logf("retained state: %s (%s)", p, d.State)
	}
}

func TestReviewR1DeactivateDeletedAlias(t *testing.T) {
	s := newServeServerForTest(t)
	root := reviewPhysicalTemp(t)
	project := filepath.Join(root, "project")
	stageSkillWithSecret(t, project, "resident")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	lexical := filepath.Join(alias, "project")
	manifest, err := s.activate(lexical)
	if err != nil {
		t.Fatal(err)
	}
	token := manifest["dir_token"].(string)
	if !s.facade.HasRoute(token, "resident") {
		t.Fatal("control: route missing")
	}
	if err := os.RemoveAll(project); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/__omac__/deactivate", strings.NewReader(fmt.Sprintf(`{"dir":%q}`, lexical)))
	addServeTestToken(req)
	w := httptest.NewRecorder()
	s.controlMux().ServeHTTP(w, req)
	t.Logf("deactivate status=%d body=%s", w.Code, w.Body.String())
	if w.Code != http.StatusOK {
		t.Fatalf("deactivate: %d", w.Code)
	}
	if s.facade.HasRoute(token, "resident") {
		t.Error("successful deactivate left the deleted project's route live")
	}
	if s.byToken[token] != nil {
		t.Error("successful deactivate left the deleted project's token live")
	}
	if len(s.dirs) != 0 {
		t.Errorf("successful deactivate left %d active directories", len(s.dirs))
	}
}
