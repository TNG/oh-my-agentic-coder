package facade

// Security regression test for SKILL.md doc-serving.
//
// serveSkillDoc must not serve a SKILL.md whose final path component is a
// symlink, or whose resolved target falls outside the skill directory. The
// lexical containment check alone is insufficient when any path component is
// caller-controlled.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSecurityServeModeSymlinkedSkillDocRejected(t *testing.T) {
	skillDir := t.TempDir()

	// A file outside the skill dir that must never be served through the
	// facade's doc-discovery path.
	outsideDir := t.TempDir()
	secretContent := "HOST-ONLY-CONTENT-7e2a"
	secretFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte(secretContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// Plant SKILL.md as a symlink to the outside file.
	if err := os.Symlink(secretFile, filepath.Join(skillDir, skillDocName)); err != nil {
		t.Fatal(err)
	}

	// UpstreamPort 1 is intentionally dead: if serveSkillDoc correctly
	// rejects the symlink, the request falls through to the proxy which
	// fails with a 5xx — proving the doc was not served.
	f := New("", "", []Route{
		{Mount: "echo", Namespace: GlobalNamespace, State: RouteReady, UpstreamPort: 1, SkillDir: skillDir},
	}, 1<<20, 0, "", "test")

	rec := httptest.NewRecorder()
	f.handle(rec, httptest.NewRequest(http.MethodGet, "/__global__/echo", nil))

	if rec.Code == http.StatusOK && rec.Header().Get("X-Omac-Discovery") == "skill-md" {
		t.Errorf("serveSkillDoc served a symlinked SKILL.md (status=%d, X-Omac-Discovery=%q); symlinked docs must be rejected", rec.Code, rec.Header().Get("X-Omac-Discovery"))
	}
	if rec.Body.String() == secretContent {
		t.Errorf("serveSkillDoc returned content from outside the skill dir via a symlink; body must not equal the escaped file content")
	}
}
