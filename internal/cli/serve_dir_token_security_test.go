package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

type dirControlResponse struct {
	status  int
	body    string
	decoded map[string]any
}

// postDirControl drives a serve control-plane handler through the real mux.
// dirToken is sent only when non-empty, so callers can exercise the
// path-only and ownership-proof cases separately.
func postDirControl(t *testing.T, base, path, dir, dirToken string) dirControlResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(fmt.Sprintf(`{"dir":%q}`, dir)))
	if err != nil {
		t.Fatalf("build %s request: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Omac-Control-Token", "test-control-token")
	if dirToken != "" {
		req.Header.Set("X-Omac-Dir-Token", dirToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s response: %v", path, err)
	}
	out := dirControlResponse{status: resp.StatusCode, body: string(raw)}
	_ = json.Unmarshal(raw, &out.decoded)
	return out
}

func firstActivation(t *testing.T, base, dir string) string {
	t.Helper()
	res := postDirControl(t, base, "/__omac__/activate", dir, "")
	if res.status != http.StatusOK {
		t.Fatalf("first activation status = %d, want 200 (body=%s)", res.status, res.body)
	}
	token, _ := res.decoded["dir_token"].(string)
	if token == "" {
		t.Fatalf("first activation returned no dir token: the fixture is broken, not the security property (body=%s)", res.body)
	}
	return token
}

// TestSecurityDirTokenWithheldFromControlPlane asserts that a later
// activation of an already-active directory never hands the namespace key or
// namespace-bearing URLs back through the control plane.
func TestSecurityDirTokenWithheldFromControlPlane(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)
	dir := t.TempDir()
	stageSkillWithSecret(t, dir, "slack")

	srv := httptest.NewServer(s.controlMux())
	t.Cleanup(srv.Close)

	token := firstActivation(t, srv.URL, dir)
	promoteToReady(t, s, dir, "slack", token, 50001)

	res := postDirControl(t, srv.URL, "/__omac__/activate", dir, token)
	if res.status != http.StatusOK {
		t.Fatalf("later activation status = %d, want 200 (body=%s)", res.status, res.body)
	}
	if _, present := res.decoded["dir_token"]; present {
		t.Errorf("later activation returned dir_token: the namespace key must be issued only on first activation")
	}
	if strings.Contains(res.body, token) {
		t.Errorf("a later activation response carried the live dir token in another field: the namespace is recoverable from the control plane")
	}
	skills, _ := res.decoded["skills"].([]any)
	for _, raw := range skills {
		skill, _ := raw.(map[string]any)
		if _, present := skill["base"]; present {
			t.Errorf("a later activation response disclosed base: that URL embeds the directory namespace")
		}
		if _, present := skill["socket_base"]; present {
			t.Errorf("a later activation response disclosed socket_base: that URL embeds the directory namespace")
		}
	}
}

// TestSecurityReactivationRequiresDirToken asserts that re-activating an
// existing directory requires the token issued for that exact directory.
func TestSecurityReactivationRequiresDirToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)

	t.Run("missing token", func(t *testing.T) {
		s := newServeServerForTest(t)
		dir := t.TempDir()
		stageSkillWithSecret(t, dir, "slack")
		srv := httptest.NewServer(s.controlMux())
		t.Cleanup(srv.Close)
		token := firstActivation(t, srv.URL, dir)

		res := postDirControl(t, srv.URL, "/__omac__/activate", dir, "")
		if res.status != http.StatusForbidden {
			t.Errorf("re-activation without the dir token status = %d, want 403 (body=%s)", res.status, res.body)
		}
		assertOriginalActivation(t, s, dir, token)
	})

	t.Run("stale token", func(t *testing.T) {
		s := newServeServerForTest(t)
		dir := t.TempDir()
		stageSkillWithSecret(t, dir, "slack")
		srv := httptest.NewServer(s.controlMux())
		t.Cleanup(srv.Close)
		token := firstActivation(t, srv.URL, dir)
		stale := strings.Repeat("0", len(token))

		res := postDirControl(t, srv.URL, "/__omac__/activate", dir, stale)
		if res.status != http.StatusForbidden {
			t.Errorf("re-activation with a stale dir token status = %d, want 403 (body=%s)", res.status, res.body)
		}
		assertOriginalActivation(t, s, dir, token)
	})

	t.Run("another directory's token", func(t *testing.T) {
		s := newServeServerForTest(t)
		dirA := t.TempDir()
		dirB := t.TempDir()
		stageSkillWithSecret(t, dirA, "alpha")
		stageSkillWithSecret(t, dirB, "beta")
		srv := httptest.NewServer(s.controlMux())
		t.Cleanup(srv.Close)
		tokenA := firstActivation(t, srv.URL, dirA)
		tokenB := firstActivation(t, srv.URL, dirB)

		res := postDirControl(t, srv.URL, "/__omac__/activate", dirA, tokenB)
		if res.status != http.StatusForbidden {
			t.Errorf("re-activation with another directory's dir token status = %d, want 403 (body=%s)", res.status, res.body)
		}
		assertOriginalActivation(t, s, dirA, tokenA)
	})
}

// TestSecurityReloadRequiresDirToken asserts that reload is gated by the
// directory's own token and does not rotate the token when it succeeds.
func TestSecurityReloadRequiresDirToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)

	t.Run("missing token", func(t *testing.T) {
		s := newServeServerForTest(t)
		dir := t.TempDir()
		stageSkillWithSecret(t, dir, "slack")
		srv := httptest.NewServer(s.controlMux())
		t.Cleanup(srv.Close)
		token := firstActivation(t, srv.URL, dir)

		res := postDirControl(t, srv.URL, "/__omac__/reload", dir, "")
		if res.status != http.StatusForbidden {
			t.Errorf("reload without the dir token status = %d, want 403 (body=%s)", res.status, res.body)
		}
		assertOriginalActivation(t, s, dir, token)
	})

	t.Run("matching token", func(t *testing.T) {
		s := newServeServerForTest(t)
		dir := t.TempDir()
		stageSkillWithSecret(t, dir, "slack")
		srv := httptest.NewServer(s.controlMux())
		t.Cleanup(srv.Close)
		token := firstActivation(t, srv.URL, dir)

		res := postDirControl(t, srv.URL, "/__omac__/reload", dir, token)
		if res.status != http.StatusOK {
			t.Fatalf("reload with the dir token status = %d, want 200 (body=%s)", res.status, res.body)
		}
		assertOriginalActivation(t, s, dir, token)
		if strings.Contains(res.body, token) {
			t.Errorf("reload response carried the live dir token: the namespace key must not be re-issued")
		}
	})
}

// TestSecurityDeactivateRequiresDirToken asserts that deactivation requires
// the directory's own token.
func TestSecurityDeactivateRequiresDirToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)

	t.Run("missing token", func(t *testing.T) {
		s := newServeServerForTest(t)
		dir := t.TempDir()
		stageSkillWithSecret(t, dir, "slack")
		srv := httptest.NewServer(s.controlMux())
		t.Cleanup(srv.Close)
		token := firstActivation(t, srv.URL, dir)

		res := postDirControl(t, srv.URL, "/__omac__/deactivate", dir, "")
		if res.status != http.StatusForbidden {
			t.Errorf("deactivate without the dir token status = %d, want 403 (body=%s)", res.status, res.body)
		}
		assertOriginalActivation(t, s, dir, token)
	})

	t.Run("matching token", func(t *testing.T) {
		s := newServeServerForTest(t)
		dir := t.TempDir()
		stageSkillWithSecret(t, dir, "slack")
		srv := httptest.NewServer(s.controlMux())
		t.Cleanup(srv.Close)
		token := firstActivation(t, srv.URL, dir)

		res := postDirControl(t, srv.URL, "/__omac__/deactivate", dir, token)
		if res.status != http.StatusOK {
			t.Fatalf("deactivate with the dir token status = %d, want 200 (body=%s)", res.status, res.body)
		}
		s.mu.RLock()
		_, stillActive := s.dirs[dir]
		_, stillMapped := s.byToken[token]
		s.mu.RUnlock()
		if stillActive || stillMapped {
			t.Errorf("deactivate with the matching token did not remove the directory and its token")
		}
	})
}

func assertOriginalActivation(t *testing.T, s *serveServer, dir, token string) {
	t.Helper()
	s.mu.RLock()
	d, ok := s.dirs[dir]
	got := ""
	if ok {
		got = d.Token
	}
	s.mu.RUnlock()
	if !ok {
		t.Fatalf("the directory is no longer active after a rejected control-plane request")
	}
	if got != token {
		t.Errorf("the directory token changed from %q to %q after a rejected control-plane request", token, got)
	}
}
