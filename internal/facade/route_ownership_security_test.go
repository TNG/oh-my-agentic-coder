package facade

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// These tests pin the ownership rules for mounts that already serve live
// traffic. A route's Owner names the skill that installed it, and AddRoute
// uses that field to tell two situations apart: the owning skill updating
// its own route (allowed) and some other registration claiming the mount
// (refused). Reload installs not-ready placeholders with no owner, so an
// unowned route must never displace an owned one.

// startOwnershipUpstream runs a loopback server that answers every request
// with the given body, and returns its TCP port.
func startOwnershipUpstream(t *testing.T, body string) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %s: %v", srv.URL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %s: %v", srv.URL, err)
	}
	return port
}

func TestSecurityReloadCannotReplaceOwnedRoute(t *testing.T) {
	answerA := startOwnershipUpstream(t, "answer-a")
	answerB := startOwnershipUpstream(t, "answer-b")

	f := New("", "127.0.0.1:0", []Route{{
		Mount:        "keeper",
		UpstreamPort: answerA,
		Skill:        "keeper",
		Owner:        "keeper",
		State:        RouteReady,
	}}, 0, 30*time.Second, filepath.Join(t.TempDir(), "facade-access.log"), "route-ownership-test")
	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("start facade: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	base := "http://127.0.0.1:" + strconv.Itoa(f.TCPPort())

	probe := func(mount string) (int, string) {
		t.Helper()
		resp, err := http.Get(base + "/" + mount + "/data")
		if err != nil {
			t.Fatalf("probe /%s/data: %v", mount, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// Fixture control: the owned route must actually serve, or the
	// assertions below prove nothing.
	if code, body := probe("keeper"); code != http.StatusOK || body != "answer-a" {
		t.Fatalf("fixture: mount keeper answers %d %q, want 200 %q", code, body, "answer-a")
	}

	// A live route owned by a different skill must be refused.
	f.AddRoute(Route{
		Mount:        "keeper",
		UpstreamPort: answerB,
		Skill:        "latecomer",
		Owner:        "latecomer",
		State:        RouteReady,
	})
	if code, body := probe("keeper"); code != http.StatusOK || body != "answer-a" {
		t.Errorf("a route owned by another skill replaced the live route at mount keeper (now %d %q)", code, body)
	}
	if got := f.UpstreamPort("", "keeper"); got != answerA {
		t.Errorf("UpstreamPort(keeper) = %d after a differently-owned AddRoute, want %d", got, answerA)
	}

	// An unowned route must be refused as well. Not-ready placeholders
	// carry no owner, so this is the case reload exercises.
	f.AddRoute(Route{
		Mount:  "keeper",
		Skill:  "latecomer",
		State:  RouteBroken,
		Detail: "the host has not approved this skill",
	})
	if code, body := probe("keeper"); code != http.StatusOK || body != "answer-a" {
		t.Errorf("an unowned placeholder replaced the live route at mount keeper (now %d %q)", code, body)
	}
	if got := f.UpstreamPort("", "keeper"); got != answerA {
		t.Errorf("UpstreamPort(keeper) = %d after an unowned AddRoute, want %d", got, answerA)
	}

	// The guard must not overreach. A placeholder may install on a mount
	// nobody owns yet, and the owning skill's live route may replace it:
	// that is how a route comes up once approval or credentials arrive.
	f.AddRoute(Route{
		Mount:  "fresh",
		Skill:  "fresh",
		State:  RouteBroken,
		Detail: "waiting for approval",
	})
	if code, _ := probe("fresh"); code != http.StatusBadGateway {
		t.Fatalf("fixture: placeholder at mount fresh answers %d, want 502", code)
	}
	f.AddRoute(Route{
		Mount:        "fresh",
		UpstreamPort: answerA,
		Skill:        "fresh",
		Owner:        "fresh",
		State:        RouteReady,
	})
	if code, body := probe("fresh"); code != http.StatusOK || body != "answer-a" {
		t.Errorf("an owned route could not replace the placeholder at mount fresh (got %d %q)", code, body)
	}
	if got := f.UpstreamPort("", "fresh"); got != answerA {
		t.Errorf("UpstreamPort(fresh) = %d after the upgrade, want %d", got, answerA)
	}
}
