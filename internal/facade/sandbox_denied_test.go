package facade

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubChecker is a minimal ProtectedPathChecker for testing.
type stubChecker struct {
	protected map[string]string // path -> rule
}

func (s stubChecker) IsProtected(absPath string) (rule string, ok bool) {
	r, ok := s.protected[absPath]
	return r, ok
}

func TestSandboxDeniedProtected(t *testing.T) {
	f := &Facade{
		ProtectedPathChecker: stubChecker{protected: map[string]string{
			"/home/u/.aws/credentials": "baseline",
		}},
	}
	req := httptest.NewRequest(http.MethodGet, "/sandbox/denied?path=/home/u/.aws/credentials", nil)
	w := httptest.NewRecorder()
	f.handleSandboxDenied(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	if !strings.Contains(w.Header().Get("X-Omac-Sandbox"), "denied") {
		t.Errorf("missing X-Omac-Sandbox header")
	}
	var resp struct {
		Denied bool   `json:"denied"`
		Path   string `json:"path"`
		Rule   string `json:"rule"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Denied {
		t.Error("denied = false; want true")
	}
	if resp.Rule != "baseline" {
		t.Errorf("rule = %q; want baseline", resp.Rule)
	}
	if !strings.Contains(strings.ToLower(resp.Note), "intentionally") {
		t.Errorf("note lacks deterrent wording: %q", resp.Note)
	}
}

func TestSandboxDeniedNotProtected(t *testing.T) {
	f := &Facade{
		ProtectedPathChecker: stubChecker{protected: map[string]string{}},
	}
	req := httptest.NewRequest(http.MethodGet, "/sandbox/denied?path=/tmp/random", nil)
	w := httptest.NewRecorder()
	f.handleSandboxDenied(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", w.Code)
	}
	var resp struct {
		Denied bool `json:"denied"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Denied {
		t.Error("denied = true; want false")
	}
}

func TestSandboxDeniedNoChecker(t *testing.T) {
	f := &Facade{}
	req := httptest.NewRequest(http.MethodGet, "/sandbox/denied?path=/x", nil)
	w := httptest.NewRecorder()
	f.handleSandboxDenied(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", w.Code)
	}
}

func TestSandboxDeniedMissingPath(t *testing.T) {
	f := &Facade{
		ProtectedPathChecker: stubChecker{protected: map[string]string{}},
	}
	req := httptest.NewRequest(http.MethodGet, "/sandbox/denied", nil)
	w := httptest.NewRecorder()
	f.handleSandboxDenied(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", w.Code)
	}
}
