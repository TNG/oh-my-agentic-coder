package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// TestSecurityActivateSymlinkOutsideRootsRefused pins serve mode's --root
// activation policy to the filesystem rather than to the path string: the
// policy must be enforced on the resolved directory, and a link as the
// final path component must never be accepted. A link planted inside an
// allowed root must therefore not bring a directory outside every root
// under serve's management, together with the host-side registry writes
// that activation performs in the activated tree.
func TestSecurityActivateSymlinkOutsideRootsRefused(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t) // isolates HOME/XDG

	// Roots are stored in resolved form, as the flag-parse normalization
	// produces them; every path below is created under that form so the
	// containment comparison is like-for-like.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	s.roots = []string{root}

	// A directory foreign to the policy, holding a registrable skill so a
	// successful activation would leave registry state behind in it.
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve outside dir: %v", err)
	}
	stageSkillWithSecret(t, outside, "notifier")
	before := treeFiles(t, outside)

	link := filepath.Join(root, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Control: the policy refuses the foreign directory by its plain path.
	if _, err := s.activate(outside); err == nil {
		t.Fatalf("control: activate on the foreign directory was not refused: the fixture is broken, not the security property")
	}

	// Control: a plain directory inside the root still activates, so the
	// assertions below are about the link, not about a broken policy.
	project := filepath.Join(root, "project")
	stageSkillWithSecret(t, project, "deployer")
	manifest, err := s.activate(project)
	if err != nil {
		t.Fatalf("control: activate on a plain directory inside the root failed: %v", err)
	}
	if token, _ := manifest["dir_token"].(string); token == "" {
		t.Fatalf("control: activation issued no dir_token: the fixture is broken, not the security property")
	}

	// Activation through the link must be refused.
	if m, aerr := s.activate(link); aerr == nil {
		token, _ := m["dir_token"].(string)
		t.Errorf("activate on %s succeeded and issued dir_token %q: the roots policy must be enforced on the resolved directory, so a link inside an allowed root must not admit a directory outside every root", link, token)
	}
	assertActiveDirs(t, s, project)

	// Refusal must also mean no host-side writes into the foreign tree.
	if after := treeFiles(t, outside); !slices.Equal(before, after) {
		t.Errorf("files appeared in the foreign directory after the refused activation: %v", after)
	}

	// Defense in depth: even a link whose target lies inside the roots must
	// be refused, because the final path component is a link.
	inside := filepath.Join(root, "inside")
	stageSkillWithSecret(t, inside, "publisher")
	linkInside := filepath.Join(root, "linked-inside")
	if err := os.Symlink(inside, linkInside); err != nil {
		t.Fatalf("symlink inside roots: %v", err)
	}
	if _, err := s.activate(linkInside); err == nil {
		t.Errorf("activate on %s succeeded: a link as the final path component must be refused even when its target lies inside the roots", linkInside)
	}
	assertActiveDirs(t, s, project)

	// The teardown endpoint applies the same discipline: naming the link
	// must be refused, not reported as a successful deactivation.
	control := httptest.NewServer(s.controlMux())
	t.Cleanup(control.Close)
	req, err := http.NewRequest(http.MethodPost, control.URL+"/__omac__/deactivate",
		strings.NewReader(fmt.Sprintf("{%q:%q}", "dir", link)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	addServeTestToken(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /__omac__/deactivate: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		t.Errorf("POST /__omac__/deactivate for %s reported success (%s): a link path must be refused by the same policy as activation", link, strings.TrimSpace(string(respBody)))
	}
	assertActiveDirs(t, s, project)
}

// treeFiles lists every file under dir, sorted, for before/after comparison.
func treeFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	slices.Sort(files)
	return files
}

// assertActiveDirs fails unless serve's active set is exactly want.
func assertActiveDirs(t *testing.T, s *serveServer, want ...string) {
	t.Helper()
	s.mu.RLock()
	active := make([]string, 0, len(s.dirs))
	for dir := range s.dirs {
		active = append(active, dir)
	}
	s.mu.RUnlock()
	slices.Sort(active)
	slices.Sort(want)
	if !slices.Equal(active, want) {
		t.Errorf("serve's active directories are %v, want exactly %v: a refused activation must leave the active set untouched", active, want)
	}
}
