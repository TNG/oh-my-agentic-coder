//go:build linux

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/gitservice"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

func TestGitServiceUsesFinalScanDecision(t *testing.T) {
	requireGitServiceWorker(t)
	for _, choice := range []sandboxrun.ScanChoice{sandboxrun.ScanContinue, sandboxrun.ScanStartIncomplete} {
		t.Run(map[sandboxrun.ScanChoice]string{sandboxrun.ScanContinue: "continue", sandboxrun.ScanStartIncomplete: "incomplete"}[choice], func(t *testing.T) {
			isolateHome(t)
			root, p := overLimitWorkdir(t, 150)
			p.Filesystem.OverrideDeny = nil
			c := exec.Command("/usr/bin/git", "init", "-q", "-b", "main", root)
			if out, err := c.CombinedOutput(); err != nil {
				t.Fatalf("init: %v %s", err, out)
			}
			writeProtectedFile(t, root, "nested/.env")
			s, err := gitservice.New(gitservice.Options{Workdir: root, Name: "Test", Email: "test@example.invalid"})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			calls := stubScanAsk(t, choice)
			plan := sandboxPlan{Native: true, Policy: p}
			argv := []string{"/omac", "sandbox", "run", "--git-read-only", "--deny-literal", s.RuntimeDir(), "--", "true"}
			if err := configureGitService(s, plan, argv); err == nil {
				t.Fatal("unresolved scan limit accepted")
			}
			w, argv, err := startProtectedWatch(&Env{}, audit.Nop(), nil, plan, argv, root, sandboxrun.WatchTuning{})
			if err != nil {
				t.Fatal(err)
			}
			defer reportText(w)
			if *calls != 1 {
				t.Fatalf("scan prompts=%d", *calls)
			}
			err = configureGitService(s, plan, argv)
			status := httptest.NewRecorder()
			s.ServeHTTP(status, httptest.NewRequest("GET", "/status", nil))
			if choice == sandboxrun.ScanStartIncomplete {
				if err == nil || !strings.Contains(err.Error(), "complete secret-file scan") || status.Code != 503 {
					t.Fatalf("incomplete scan: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				var result gitservice.Result
				if err := json.Unmarshal(status.Body.Bytes(), &result); err != nil || status.Code != 200 || result.State == nil {
					t.Fatalf("configured status: %d %s", status.Code, status.Body)
				}
				body, _ := json.Marshal(gitservice.Request{ID: fmt.Sprintf("%s:%032x", result.State.Session, 1), Expected: result.State, Paths: []string{"nested/.env"}})
				staged := httptest.NewRecorder()
				s.ServeHTTP(staged, httptest.NewRequest("POST", "/stage", bytes.NewReader(body)))
				if staged.Code != 409 || !strings.Contains(staged.Body.String(), "path is denied by the sandbox") {
					t.Fatalf("worker policy omitted a scanned secret: %d %s", staged.Code, staged.Body)
				}
				if err = s.ConfigureDenies([]string{filepath.Join(root, "nested/.env")}); err == nil {
					t.Fatal("policy not finalized")
				}
			}
		})
	}
}

func TestGitServicePolicyRequiresMetadataProtection(t *testing.T) {
	argv := []string{"/omac", "sandbox", "run", "--", "true"}
	if err := configureGitService(nil, sandboxPlan{}, argv); err == nil || !strings.Contains(err.Error(), "read-only Git metadata") {
		t.Fatalf("unprotected launch: %v", err)
	}
}
