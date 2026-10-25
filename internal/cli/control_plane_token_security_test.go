package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// The serve-mode directory token.
//
// `omac serve` hosts many workdirs behind one facade port. A session's skills
// are reachable only under its directory token, a random 16-byte value minted
// at activation: the token is the namespace key and the whole of the access
// control between one workdir's skills and another's. Those skills can hold
// API credentials, so a session holding a second session's token has that
// session's reach.
//
// The control plane's port is opened into every sandbox on purpose — the
// harness plugin activates and reloads through it, presenting the control
// token it was handed via the environment. GET /__omac__/dirs requires the
// control token and lists directories without their dir tokens. Re-activating
// a directory that is already active reaches the same token by a different
// route: the caller names a directory it does not own, and the response is
// that directory's live manifest, token included.
//
// Nothing about a POST identifies its sender. The session that owns the
// directory was handed its token when it activated and has no need to be told
// again, so re-issuing it can only ever inform someone else.

// TestSecurityActivateDoesNotDiscloseDirToken asserts that activating a
// directory that is already active does not hand its token to the caller.
func TestSecurityActivateDoesNotDiscloseDirToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)

	victimDir := t.TempDir()
	stageSkillWithSecret(t, victimDir, "slack")

	// The victim's own session activates first and receives the token, which
	// is how it reaches its own skills.
	manifest, err := s.activate(victimDir)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	token, _ := manifest["dir_token"].(string)
	if token == "" {
		t.Fatal("the first activation returned no dir token: the fixture is broken, not the security property")
	}

	control := httptest.NewServer(s.controlMux())
	t.Cleanup(control.Close)

	// Control: the control-plane holder (the operator who started serve)
	// can list the active directories, and the listing must not include
	// the tokens.
	dirsStatus, dirsRaw := controlRoundTrip(t, http.MethodGet, control.URL+"/__omac__/dirs", s.controlToken, "")
	if dirsStatus != http.StatusOK {
		t.Fatalf("GET /__omac__/dirs (with control token) status = %d: the fixture is broken, not the security property (body=%s)", dirsStatus, dirsRaw)
	}
	if !strings.Contains(dirsRaw, victimDir) {
		t.Fatalf("the active directory was not listed at all (%s): the fixture is broken", dirsRaw)
	}
	if strings.Contains(dirsRaw, token) {
		t.Errorf("GET /__omac__/dirs disclosed the dir token: the directory listing hands out the key to every active session's skills")
	}

	// A second session, or any local process, names the victim's directory.
	replayStatus, replayRaw := controlRoundTrip(t, http.MethodPost, control.URL+"/__omac__/activate", s.controlToken, fmt.Sprintf("{%q:%q}", "dir", victimDir))
	if replayStatus != http.StatusOK {
		t.Fatalf("control: re-activation returned %d (%s): the fixture is broken, not the security property", replayStatus, replayRaw)
	}
	replay := jsonStrField(replayRaw, "dir_token")

	// Asserting absence rather than difference: a token that is merely
	// rotated on re-activation is still a live namespace key handed to a
	// caller that proved nothing, and it additionally cuts off the session
	// that owns the directory.
	if replay != "" {
		t.Errorf("re-activating an already-active directory returned the live token %q: a caller that knows only the path gains that session's skills, credentials included", replay)
	}
}
