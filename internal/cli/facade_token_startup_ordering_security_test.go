package cli

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// The facade's TCP auth gate reads f.FacadeToken on every request and only
// enforces the bearer-token check once the field is non-empty. If the token
// is assigned after f.Start returns, there is a window — however short —
// where the listener is accepting TCP connections but the gate is disabled,
// so tokenless requests are served. The blocking runServe/runLaunch
// functions create the facade internally, so the ordering cannot be observed
// from outside at runtime; these tests guard it at the source level and
// verify the behavioral consequence (tokenless TCP → 401) when the token is
// set before Start, mirroring the fixed wiring.

// tokenAssignmentPrecedesStart reads the named source file and asserts that
// the f.FacadeToken assignment appears textually before the f.Start(ctx)
// call. The fix moves the assignment above Start; this guard fails if the
// lines are ever reordered back.
func tokenAssignmentPrecedesStart(t *testing.T, filename string) {
	t.Helper()
	src, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	text := string(src)
	tokenIdx := strings.Index(text, "f.FacadeToken =")
	startIdx := strings.Index(text, "f.Start(ctx)")
	if tokenIdx < 0 {
		t.Fatalf("%s: f.FacadeToken assignment not found — source pattern changed; update this guard", filename)
	}
	if startIdx < 0 {
		t.Fatalf("%s: f.Start(ctx) call not found — source pattern changed; update this guard", filename)
	}
	if tokenIdx > startIdx {
		t.Errorf("%s: f.FacadeToken is assigned after f.Start(ctx) (offset %d > %d): the auth gate is disabled between listener-start and token-assignment, so tokenless TCP requests are served during startup", filename, tokenIdx, startIdx)
	}
}

// assertTokenlessTCPRejected verifies that a facade with the token set
// before Start rejects tokenless TCP requests with 401 from the first
// connection — the behavioral consequence of the correct ordering.
func assertTokenlessTCPRejected(t *testing.T) {
	t.Helper()
	f := facade.New("", "127.0.0.1:0", nil, 1<<20, 0, "", "test")
	f.FacadeToken = mintToken()
	if err := f.Start(t.Context()); err != nil {
		t.Fatalf("facade start: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	port := f.TCPPort()
	if port == 0 {
		t.Skip("no TCP listener bound: nothing to gate")
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("tokenless TCP request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("tokenless TCP request got status %d, want 401: the gate must be active from the first connection when the token is set before Start", resp.StatusCode)
	}
}

// TestSecurityFacadeTokenMintedBeforeListenerStarts guards the serve path:
// the facade token must be minted and assigned before f.Start so the TCP
// auth gate is active from the first accepted connection.
func TestSecurityFacadeTokenMintedBeforeListenerStarts(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	tokenAssignmentPrecedesStart(t, "serve.go")
	assertTokenlessTCPRejected(t)
}

// TestSecurityStartMintsFacadeTokenBeforeListenerStarts guards the start
// path with the same property.
func TestSecurityStartMintsFacadeTokenBeforeListenerStarts(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	tokenAssignmentPrecedesStart(t, "start.go")
	assertTokenlessTCPRejected(t)
}
