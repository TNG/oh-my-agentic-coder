//go:build vuln

package facade

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/intent"
)

// Who may talk to the facade.
//
// The facade offers two transports for the same handler. The Unix socket is
// access-controlled by the filesystem: its directory is created 0700 and the
// socket itself chmod 0600, so the kernel turns away anyone but the owning
// user. The loopback TCP listener has no equivalent — TCP carries no peer
// credentials to check — and the facade adds no token, no session key, and no
// authentication of its own on top.
//
// The transports therefore do not offer the same thing. Everything the socket
// protects is served, unauthenticated, to whoever connects to the port:
//
//   - GET / enumerates the mounted skill routes,
//   - those routes proxy to sidecars that hold skill API tokens in their
//     environment, on ports deliberately withheld from the sandbox so the
//     facade is the only way to reach them,
//   - POST /sandbox/intent writes into the record the user is shown when
//     deciding whether to approve access.
//
// "Local process" is not a trust boundary on a machine with more than one
// account, and it is not one for a sandboxed agent either, since reaching the
// facade port is exactly what the sandbox is supposed to mediate.
//
// The test passes if the TCP transport is dropped altogether: no listener,
// nothing to authenticate.

// TestSecurityTCPListenerRequiresAuthentication asserts that a caller with no
// credentials gets nothing useful from the facade's TCP transport.
func TestSecurityTCPListenerRequiresAuthentication(t *testing.T) {
	requireLoopbackListener(t)

	s := startSidecar(t, "HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\nsecret")
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)

	f := New("", "127.0.0.1:0", []Route{{
		Mount:        "slack",
		UpstreamPort: s.port,
		Skill:        "slack",
		State:        RouteReady,
	}}, 0, time.Minute, "", "test")
	f.IntentRegistry = reg
	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("facade start: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	port := f.TCPPort()
	if port == 0 {
		return // no TCP transport: nothing is exposed without credentials
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 5 * time.Second}

	// Control: the facade is up and answering on this port. Without it, a
	// facade that failed to start would satisfy every assertion below.
	resp, err := client.Get(base + "/sandbox/denied?path=/etc/shadow")
	if err != nil {
		t.Fatalf("the facade is not answering on its own TCP port (%v): the fixture is broken, not the security property", err)
	}
	resp.Body.Close()

	t.Run("route enumeration", func(t *testing.T) {
		resp, err := client.Get(base + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer resp.Body.Close()
		var status struct {
			Skills []string `json:"skills"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&status)
		if len(status.Skills) > 0 {
			t.Errorf("an unauthenticated caller listed the mounted skills %v: any local process learns what this session has access to", status.Skills)
		}
	})

	t.Run("sidecar proxying", func(t *testing.T) {
		resp, err := client.Get(base + "/slack/whoami")
		if err != nil {
			t.Fatalf("GET /slack/whoami: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("an unauthenticated caller reached a skill sidecar through the facade: the sidecar's credentials are usable by any local process, which is what withholding its port from the sandbox was meant to prevent")
		}
	})

	t.Run("intent writes", func(t *testing.T) {
		body := strings.NewReader(`{"target":"attacker.example","reason":"planted by an unauthenticated caller"}`)
		resp, err := client.Post(base+"/sandbox/intent", "application/json", body)
		if err != nil {
			t.Fatalf("POST /sandbox/intent: %v", err)
		}
		defer resp.Body.Close()
		if e, ok := reg.Lookup("attacker.example"); ok {
			t.Errorf("an unauthenticated caller wrote an intent record (%q): the text shown to the user when approving access is writable by anyone on the machine", e.Reason)
		}
	})
}
