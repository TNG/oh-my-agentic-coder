package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/bridge"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
)

type mcpAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (a *mcpAudit) Emit(ev audit.Event) {
	a.mu.Lock()
	a.events = append(a.events, ev)
	a.mu.Unlock()
}
func (a *mcpAudit) Close() error    { return nil }
func (a *mcpAudit) RunID() string   { return "" }
func (a *mcpAudit) NextSeq() uint64 { return 0 }

func (a *mcpAudit) routeStates() []audit.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []audit.Event
	for _, ev := range a.events {
		if ev.Type == audit.TypeRouteState {
			out = append(out, ev)
		}
	}
	return out
}

// mcpFixture runs the real gateways, facade and watcher of one launch without
// a harness.
type mcpFixture struct {
	*mcpRig
	plan   *mcpPlan
	routes []facade.Route
	f      *facade.Facade
	aud    *mcpAudit
	stderr func() string
}

func newMCPFixture(t *testing.T, names ...string) *mcpFixture {
	t.Helper()
	old := mcpRespawnBackoff
	mcpRespawnBackoff = 20 * time.Millisecond
	t.Cleanup(func() { mcpRespawnBackoff = old })

	r := newMCPRig(t)
	for _, name := range names {
		r.register(name, true, "claude-code")
	}
	harness, _ := config.LookupHarness("claude-code")
	env, stderr := launchTestEnv(t, r.wd)
	lc := config.LauncherConfig{}
	lc.Facade.BaseEnvPassthrough = []string{"PATH", "HOME"}
	plan, code := prepareMCP(env, harness, lc, launchOpts{label: "start", harness: harness})
	if code != ExitOK || plan == nil {
		t.Fatalf("prepareMCP: code %d plan %v\n%s", code, plan, stderr())
	}
	fx := &mcpFixture{mcpRig: r, plan: plan, aud: &mcpAudit{}, stderr: stderr}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var err error
	fx.routes, err = plan.launchMCPGateways(ctx, env, "omac start", lc, fx.aud, filepath.Join(r.shortTmp, "rt"))
	if err != nil {
		t.Fatalf("launchMCPGateways: %v", err)
	}
	fx.f = facade.New(filepath.Join(r.shortTmp, "f.sock"), "127.0.0.1:0", fx.routes, 0, 0, "", "test")
	fx.f.FacadeToken = "facade-token"
	if err := fx.f.Start(ctx); err != nil {
		t.Fatal(err)
	}
	plan.watch(ctx, fx.f)
	t.Cleanup(func() {
		plan.shutdown()
		fx.f.Close()
	})
	return fx
}

func (fx *mcpFixture) server(name string) *mcpServer {
	for _, s := range fx.plan.servers {
		if s.name == name {
			return s
		}
	}
	fx.t.Fatalf("no server %q", name)
	return nil
}

func (fx *mcpFixture) pid(name string) int {
	s := fx.server(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running.Cmd.Process.Pid
}

// killGateway kills the gateway and waits until the process is gone, so the
// next request meets a closed port instead of a dying listener.
func (fx *mcpFixture) killGateway(name string) {
	fx.t.Helper()
	s := fx.server(name)
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if err := syscall.Kill(running.Cmd.Process.Pid, syscall.SIGKILL); err != nil {
		fx.t.Fatal(err)
	}
	select {
	case <-running.Done():
	case <-time.After(10 * time.Second):
		fx.t.Fatal("killed gateway did not exit")
	}
}

func (fx *mcpFixture) waitNewGateway(name string, old int) {
	fx.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for fx.pid(name) == old {
		if time.Now().After(deadline) {
			fx.t.Fatal("gateway was not respawned")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// bridgeClient drives the real bridge over a pipe.
type bridgeClient struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan string
	done  chan struct{}
}

type lineSink struct{ ch chan string }

func (s lineSink) Write(p []byte) (int, error) {
	for _, l := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		s.ch <- l
	}
	return len(p), nil
}

func (fx *mcpFixture) bridge(name string) *bridgeClient {
	pr, pw := io.Pipe()
	c := &bridgeClient{t: fx.t, in: pw, lines: make(chan string, 64), done: make(chan struct{})}
	getenv := func(k string) string {
		return map[string]string{
			"OMAC_BASE":         fmt.Sprintf("http://127.0.0.1:%d/", fx.f.TCPPort()),
			"OMAC_FACADE_TOKEN": fx.f.FacadeToken,
		}[k]
	}
	go func() {
		defer close(c.done)
		_ = bridge.Run(context.Background(), name, pr, lineSink{c.lines}, getenv)
	}()
	fx.t.Cleanup(func() {
		pw.Close()
		<-c.done
	})
	c.call(1, "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}`)
	fmt.Fprintln(pw, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return c
}

// start sends a request without waiting for the answer.
func (c *bridgeClient) start(id int, method, params string) {
	if params == "" {
		params = "{}"
	}
	fmt.Fprintf(c.in, `{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`+"\n", id, method, params)
}

// await returns the line answering id.
func (c *bridgeClient) await(id int) string {
	c.t.Helper()
	want := fmt.Sprintf(`"id":%d`, id)
	timeout := time.After(30 * time.Second)
	for {
		select {
		case l := <-c.lines:
			if strings.Contains(l, want) {
				return l
			}
		case <-timeout:
			c.t.Fatalf("no answer for request %d", id)
		}
	}
}

func (c *bridgeClient) call(id int, method, params string) string {
	c.t.Helper()
	c.start(id, method, params)
	return c.await(id)
}

func (c *bridgeClient) mustList(id int) {
	c.t.Helper()
	if got := c.call(id, "tools/list", ""); !strings.Contains(got, `"whoami"`) {
		c.t.Fatalf("tools/list = %s", got)
	}
}

func TestMCPRoutesCarryPolicyAndOwner(t *testing.T) {
	fx := newMCPFixture(t, "demo")
	if len(fx.routes) != 1 {
		t.Fatalf("routes = %d", len(fx.routes))
	}
	rt := fx.routes[0]
	if rt.Owner != "mcp:demo" || rt.MCP == nil || rt.MCP.Server != "demo" || rt.MCP.Tools["whoami"] != mcppolicy.Read {
		t.Errorf("route = %+v", rt)
	}
	if rt.Upstream.Port == 0 || rt.Upstream.Secret == "" {
		t.Errorf("route has no upstream: %+v", rt.Upstream)
	}
}

func TestMCPGatewayKilledThenNextBridgeCallSucceeds(t *testing.T) {
	fx := newMCPFixture(t, "demo")
	c := fx.bridge("demo")
	c.mustList(2)
	before, _ := fx.f.Upstream(mcpreg.FacadeNamespace, "demo")
	old := fx.pid("demo")

	fx.killGateway("demo")
	c.mustList(3)

	after, _ := fx.f.Upstream(mcpreg.FacadeNamespace, "demo")
	if after.Port == before.Port && after.Secret == before.Secret || after.Secret == before.Secret {
		t.Errorf("respawn reused the secret or port: %+v -> %+v", before, after)
	}
	if fx.pid("demo") == old {
		t.Error("gateway pid unchanged")
	}
}

func TestMCPInFlightRequestAtCrashGetsErrorWithItsID(t *testing.T) {
	fx := newMCPFixture(t, "demo")
	c := fx.bridge("demo")
	c.start(7, "tools/call", `{"name":"hang","arguments":{}}`)
	time.Sleep(500 * time.Millisecond)
	fx.killGateway("demo")

	got := c.await(7)
	var resp struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(got), &resp); err != nil || resp.Error == nil {
		t.Fatalf("in-flight request was not answered with an error: %s", got)
	}
	c.mustList(8)
}

func TestMCPGivingUpMarksRouteBrokenAndAudits(t *testing.T) {
	fx := newMCPFixture(t, "demo")
	for i := 0; i <= mcpMaxRespawns; i++ {
		old := fx.pid("demo")
		fx.killGateway("demo")
		if i < mcpMaxRespawns {
			fx.waitNewGateway("demo", old)
		}
	}

	deadline := time.Now().Add(20 * time.Second)
	for len(fx.aud.routeStates()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no route.state event\n%s", fx.stderr())
		}
		time.Sleep(10 * time.Millisecond)
	}
	ev := fx.aud.routeStates()[0]
	if ev.Skill != "demo" || ev.State != string(facade.RouteBroken) || ev.Detail == "" {
		t.Errorf("event = %+v", ev)
	}

	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/%s/demo/mcp", fx.f.TCPPort(), mcpreg.FacadeNamespace), strings.NewReader(`{}`))
	req.Header.Set("X-Omac-Facade-Token", fx.f.FacadeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("X-Omac-Reason") != "skill-broken" {
		t.Errorf("broken route answered %d %q", resp.StatusCode, resp.Header.Get("X-Omac-Reason"))
	}
	if log := readLogs(t, fx.gatewayLogs()); !strings.Contains(log, "respawning") {
		t.Errorf("gateway log has no respawn lines:\n%s", log)
	}
}

func readLogs(t *testing.T, paths []string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			b.WriteString(sc.Text() + "\n")
		}
		f.Close()
	}
	return b.String()
}

func TestMCPRefreshRestartsGatewayOfChangedRegistration(t *testing.T) {
	fx := newMCPFixture(t, "demo")
	c := fx.bridge("demo")
	c.mustList(2)
	before, _ := fx.f.Upstream(mcpreg.FacadeNamespace, "demo")

	if notices := fx.plan.refresh(context.Background(), fx.f); len(notices) != 0 {
		t.Fatalf("unchanged registry produced notices: %q", notices)
	}
	if after, _ := fx.f.Upstream(mcpreg.FacadeNamespace, "demo"); after != before {
		t.Fatal("refresh restarted an unchanged gateway")
	}

	_, err := mcpreg.Update(mcpreg.ScopeFor(fx.wd), "demo", func(d *mcpreg.Definition) error {
		d.Access = mcppolicy.Access("read-only")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	notices := fx.plan.refresh(context.Background(), fx.f)
	if len(notices) != 1 || !strings.Contains(notices[0], `"demo" restarted`) {
		t.Fatalf("notices = %q\n%s", notices, fx.stderr())
	}
	after, _ := fx.f.Upstream(mcpreg.FacadeNamespace, "demo")
	if after.Port == before.Port || after.Secret == before.Secret {
		t.Errorf("gateway not replaced: %+v -> %+v", before, after)
	}
	if got := fx.server("demo").entry.Def.Access; got != "read-only" {
		t.Errorf("server runs with access %q", got)
	}
	c.mustList(3)
}

func TestMCPRefreshReportsNewServerOnce(t *testing.T) {
	fx := newMCPFixture(t, "demo")
	fx.register("extra", true, "claude-code")

	notices := fx.plan.refresh(context.Background(), fx.f)
	if len(notices) != 1 || notices[0] != "restart the session to add extra" {
		t.Fatalf("notices = %q", notices)
	}
	if fx.f.HasRoute(mcpreg.FacadeNamespace, "extra") {
		t.Error("a server registered after launch must not get a route")
	}
	if again := fx.plan.refresh(context.Background(), fx.f); len(again) != 0 {
		t.Errorf("notice repeated: %q", again)
	}
}

func TestMCPShutdownStopsWatcherAndGateway(t *testing.T) {
	fx := newMCPFixture(t, "demo")
	running := fx.server("demo").running
	fx.plan.shutdown()
	select {
	case <-running.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("gateway still running after shutdown")
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(fx.aud.routeStates()); n != 0 {
		t.Errorf("shutdown caused %d route.state events", n)
	}
	if fx.server("demo").running != running {
		t.Error("watcher respawned the gateway during shutdown")
	}
}
