package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
)

func TestReviewR1IntermediateOutsideRefused(t *testing.T) {
	s := newServeServerForTest(t)
	root, outside := reviewPhysicalTemp(t), reviewPhysicalTemp(t)
	s.roots = []string{root}
	foreign := filepath.Join(outside, "project")
	stageSkillWithSecret(t, foreign, "foreign")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := s.activate(filepath.Join(alias, "project")); err == nil {
		t.Error("intermediate symlink escaped root")
	}
	if len(s.dirs) != 0 {
		t.Error("refusal published state")
	}
	if _, err := os.Stat(filepath.Join(foreign, ".opencode", "sidecar.json")); !os.IsNotExist(err) {
		t.Errorf("refusal created registry or stat failed: %v", err)
	}
}

func TestReviewR1UnresolvableRootsFailStartup(t *testing.T) {
	for _, name := range []string{"missing", "dangling"} {
		t.Run(name, func(t *testing.T) {
			isolateHome(t)
			base := t.TempDir()
			root := filepath.Join(base, name)
			if name == "dangling" {
				if err := os.Symlink(filepath.Join(base, "absent"), root); err != nil {
					t.Fatal(err)
				}
			}
			env := makeEnv(base)
			if code := runServe([]string{"--no-inner", "--root", root}, env); code != ExitMisuse {
				t.Errorf("unresolvable root exit=%d, want %d", code, ExitMisuse)
			}
		})
	}
}

func TestReviewR1SymlinkFailuresHTTPAndAudit(t *testing.T) {
	s := newServeServerForTest(t)
	base := reviewPhysicalTemp(t)
	project := filepath.Join(base, "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(project, link); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(base, "audit.jsonl")
	a, err := audit.New(audit.Config{Enabled: true, Path: logPath, Mode: audit.ModeServe})
	if err != nil {
		t.Fatal(err)
	}
	s.auditor = a
	for _, action := range []string{"deactivate", "reload"} {
		req := httptest.NewRequest(http.MethodPost, "/__omac__/"+action, strings.NewReader(fmt.Sprintf(`{"dir":%q}`, link)))
		addServeTestToken(req)
		w := httptest.NewRecorder()
		s.controlMux().ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d", action, w.Code)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range readAuditEvents(t, logPath) {
		if event["type"] == "control.mutation" {
			if result, ok := event["result"].(string); ok && strings.HasPrefix(result, "error: ") {
				seen[event["action"].(string)] = true
			}
		}
	}
	if !seen["deactivate"] || !seen["reload"] {
		t.Errorf("missing audited errors: %v", seen)
	}
}
