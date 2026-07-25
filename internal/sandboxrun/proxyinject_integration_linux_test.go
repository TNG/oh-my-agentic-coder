//go:build linux && external_tools

package sandboxrun

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/netproxy"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// These integration tests prove that a *real tool* run inside a filtered
// bwrap sandbox reaches the network only through the omac proxy, and only
// for allowlisted hosts. They are the end-to-end counterpart to the
// in-process proxy tests (internal/netproxy) and the env-string unit tests
// (proxyinject_test.go): here curl and node actually CONNECT through the
// proxy under Landlock enforcement.
//
// Hermetic: the upstream is a loopback httptest server and the proxy filter
// resolves a fake hostname to 127.0.0.1 (the loopback-CONNECT refusal keys
// on the requested hostname, not the resolved IP — see
// netproxy/server_test.go). No real network, no LLM, no token.
//
// NOT on the default gate: the `external_tools` build tag keeps these off
// `go test ./...`, which must not depend on a curl/Node-24 runtime. The
// "External tooling (proxy egress)" CI job supplies that matrix and runs
// them with the tag.
//
// What makes the positive assertions load-bearing: Landlock allows exactly
// one destination port, the proxy's (Stage2Args emits --connect-tcp for it).
// The upstream's port is not allowlisted, so a direct dial is kernel-blocked
// and a 200 can only have arrived through the proxy. Each test also asserts
// the *proxy's own* verdict for the host, so a pass means "the filter decided
// this", not merely "the tool exited 0" — see proxyDecisions.

const (
	proxyTestAllowedHost = "repo.omac-e2e.test"
	proxyTestDeniedHost  = "blocked.omac-e2e.test"
)

// proxyDecisions records the filter's per-request verdict lines
// ("omac sandbox: net ALLOW host:port (reason)"). Asserting against these
// pins the outcome to a filter decision: a tool that fails for an unrelated
// reason (sandbox failed to launch, missing read grant, wrong port) leaves
// no DENY behind and is therefore not mistaken for a working denial.
type proxyDecisions struct {
	mu    sync.Mutex
	lines []string
}

func (d *proxyDecisions) logf(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lines = append(d.lines, fmt.Sprintf(format, args...))
}

// verdictFor returns the recorded verdict word ("ALLOW"/"DENY") for host, or
// "" when the filter never ruled on it (i.e. the request never reached the
// proxy at all).
func (d *proxyDecisions) verdictFor(host string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.lines {
		if !strings.Contains(l, host+":") {
			continue
		}
		if strings.Contains(l, "net ALLOW") {
			return "ALLOW"
		}
		if strings.Contains(l, "net DENY") {
			return "DENY"
		}
	}
	return ""
}

func (d *proxyDecisions) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.lines...)
}

// startHermeticProxy builds an omac filtering proxy that allows exactly
// proxyTestAllowedHost and resolves every hostname to 127.0.0.1, so a
// loopback httptest upstream stands in for the allowlisted repository.
func startHermeticProxy(t *testing.T) (*netproxy.Server, *proxyDecisions) {
	t.Helper()
	dec := &proxyDecisions{}
	filter := netproxy.NewFilter(netproxy.FilterConfig{
		AllowDomains: []string{proxyTestAllowedHost},
		Resolve: func(_ context.Context, _ string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		Logf: dec.logf,
	})
	srv, err := netproxy.NewServer(filter, netproxy.NewDirectDialer(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv, dec
}

// buildOmacBinary compiles the real omac binary; stage2 must be a genuine
// `omac sandbox stage2` invocation (the test binary cannot stand in).
func buildOmacBinary(t *testing.T) string {
	t.Helper()
	omac := filepath.Join(t.TempDir(), "omac")
	build := exec.Command("go", "build", "-o", omac, "github.com/tngtech/oh-my-agentic-coder/cmd/omac")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build omac: %v\n%s", err, out)
	}
	return omac
}

// ambientPoison is prepended to the child's inherited environment so the
// tests exercise omac's real env layering rather than assuming it. Each entry
// would break routing if it survived: JAVA_TOOL_OPTIONS is on the
// always-drop blocklist, while NODE_USE_ENV_PROXY and HTTP(S)_PROXY are not
// and must instead lose to the injected overlay. 192.0.2.0/24 is TEST-NET-1
// and 9 is discard, so a leak fails loudly instead of reaching anything.
var ambientPoison = []string{
	"JAVA_TOOL_OPTIONS=-Dhttp.proxyHost=192.0.2.1 -Dhttp.proxyPort=9",
	"NODE_USE_ENV_PROXY=0",
	"HTTP_PROXY=http://192.0.2.1:9",
	"HTTPS_PROXY=http://192.0.2.1:9",
	"http_proxy=http://192.0.2.1:9",
	"https_proxy=http://192.0.2.1:9",
}

// runToolThroughProxy runs tool+args inside a filtered bwrap sandbox whose
// only permitted egress is proxy's loopback port. The child environment is
// built the way sandboxrun.Run does — sandboxprofile.FilterEnv over the
// inherited environ with injected overlaid — so the blocklist-then-overlay
// ordering that makes injection work is part of what is under test, not an
// assumption. Returns combined output and exit code.
func runToolThroughProxy(t *testing.T, omac string, proxy *netproxy.Server, injected map[string]string, tool string, args ...string) (string, int) {
	t.Helper()
	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeFiltered},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Landlock allows the proxy port (Stage2Args emits --connect-tcp for it);
	// grant read access to the omac binary and the tool's install dir.
	g.ProxyPort = proxy.Port()
	g.ReadPaths = append(g.ReadPaths, filepath.Dir(omac), filepath.Dir(tool))
	if resolved, rerr := filepath.EvalSymlinks(tool); rerr == nil {
		g.ReadPaths = append(g.ReadPaths, filepath.Dir(resolved))
	}

	stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
	argvTail := append(append([]string{}, stage2...), "--", tool)
	argvTail = append(argvTail, args...)
	argv, err := BuildBwrapArgv(g, argvTail)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = sandboxprofile.FilterEnv(append(ambientPoison, os.Environ()...), nil, injected)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("exec: %v (%s)", err, out)
	}
	return string(out), 0
}

// injectedEnv merges the proxy vars with any proxy_injection families, exactly
// as Run does before handing them to FilterEnv.
func injectedEnv(t *testing.T, proxy *netproxy.Server, families ...string) map[string]string {
	t.Helper()
	merged := proxy.EnvVars()
	if len(families) > 0 {
		inj, err := ProxyInjectionEnv(families, proxy.ProxyURL())
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range inj {
			merged[k] = v
		}
	}
	return merged
}

// requireLandlockNet gates on the kernel feature the whole test rests on.
// It fails in CI rather than skipping: a runner image without Landlock ABI 4
// would otherwise turn this job green while testing nothing.
func requireLandlockNet(t *testing.T) {
	t.Helper()
	if !LandlockNetSupported() {
		skipOrFailCI(t, "Landlock ABI %d < 4: cannot enforce per-port egress, so a 200 would not prove proxy routing", LandlockABI())
	}
}

// requireTool resolves a tool the job's matrix promises. Missing means the
// runner is misconfigured, so CI fails instead of silently dropping coverage.
func requireTool(t *testing.T, name, why string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		skipOrFailCI(t, "%s not installed (%s)", name, why)
	}
	return path
}

// TestIntegrationCurlThroughOmacProxy proves a proxy-aware tool (curl,
// which honors HTTP(S)_PROXY) reaches an allowlisted host through the omac
// proxy under a filtered sandbox, and that a non-allowlisted host is denied
// by the filter (403 on CONNECT) rather than silently reachable.
func TestIntegrationCurlThroughOmacProxy(t *testing.T) {
	requireBwrap(t)
	requireLandlockNet(t)
	curl := requireTool(t, "curl", "the proxy-aware egress case needs it")

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "allowlisted-ok")
	}))
	defer upstream.Close()
	port := upstreamPort(t, upstream.URL)

	omac := buildOmacBinary(t)
	proxy, dec := startHermeticProxy(t)
	env := injectedEnv(t, proxy)

	get := func(host string) (string, int) {
		return runToolThroughProxy(t, omac, proxy, env, curl,
			"-sS", "-k", "--max-time", "5",
			"-o", "/dev/null", "-w", "%{http_code}",
			fmt.Sprintf("https://%s:%d/", host, port))
	}

	out, code := get(proxyTestAllowedHost)
	if code != 0 || !strings.Contains(out, "200") {
		t.Errorf("allowlisted host: want HTTP 200 via proxy, got code=%d out=%q", code, out)
	}
	if v := dec.verdictFor(proxyTestAllowedHost); v != "ALLOW" {
		t.Errorf("filter verdict for %s = %q, want ALLOW (decisions: %v)", proxyTestAllowedHost, v, dec.all())
	}

	// The denial must come from the filter: curl reports the proxy's 403 on
	// the CONNECT tunnel (exit 56), and the filter records a DENY. Asserting
	// only "not 200" would also pass if the sandbox never launched.
	out, code = get(proxyTestDeniedHost)
	if code == 0 {
		t.Errorf("non-allowlisted host: curl succeeded (out=%q); the filter should deny it", out)
	}
	if !strings.Contains(out, "403") {
		t.Errorf("non-allowlisted host: want the proxy's 403 on CONNECT, got code=%d out=%q", code, out)
	}
	if v := dec.verdictFor(proxyTestDeniedHost); v != "DENY" {
		t.Errorf("filter verdict for %s = %q, want DENY (decisions: %v)", proxyTestDeniedHost, v, dec.all())
	}
}

// TestIntegrationNodeFetchThroughOmacProxy proves the `node` proxy_injection
// family works end-to-end: Node's built-in fetch (undici) ignores
// HTTP(S)_PROXY by default, so under a filtered sandbox it would dial direct
// and be blocked by Landlock. With NODE_USE_ENV_PROXY=1 (what the node
// injector sets) the native fetch routes through the omac proxy and reaches
// the allowlisted host. Requires Node >= 24.
func TestIntegrationNodeFetchThroughOmacProxy(t *testing.T) {
	requireBwrap(t)
	requireLandlockNet(t)
	node := requireTool(t, "node", "the node proxy_injection family needs Node >= 24")
	if major := nodeMajor(t, node); major < 24 {
		skipOrFailCI(t, "node %d < 24: NODE_USE_ENV_PROXY is a no-op, cannot exercise the node family", major)
	}

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "allowlisted-ok")
	}))
	defer upstream.Close()
	port := upstreamPort(t, upstream.URL)

	omac := buildOmacBinary(t)
	proxy, dec := startHermeticProxy(t)

	// Inject the real node family, so the var under test is the one the
	// profile wires up. NODE_TLS_REJECT_UNAUTHORIZED accepts the httptest
	// self-signed cert (curl uses -k for the same reason).
	env := injectedEnv(t, proxy, sandboxprofile.ProxyInjectNode)
	env["NODE_TLS_REJECT_UNAUTHORIZED"] = "0"

	script := `const u=process.argv[1];` +
		`fetch(u).then(async r=>{console.log('STATUS',r.status,await r.text());process.exit(0)})` +
		`.catch(e=>{console.error('ERR',e.message,'|',e.cause&&(e.cause.message||String(e.cause)));process.exit(1)});`

	fetch := func(host string) (string, int) {
		return runToolThroughProxy(t, omac, proxy, env, node,
			"-e", script, fmt.Sprintf("https://%s:%d/", host, port))
	}

	out, code := fetch(proxyTestAllowedHost)
	if code != 0 || !strings.Contains(out, "STATUS 200") {
		t.Errorf("node fetch of allowlisted host: want STATUS 200 via proxy, got code=%d out=%q", code, out)
	}
	if v := dec.verdictFor(proxyTestAllowedHost); v != "ALLOW" {
		t.Errorf("filter verdict for %s = %q, want ALLOW (decisions: %v)", proxyTestAllowedHost, v, dec.all())
	}

	// undici reports a denied CONNECT generically ("fetch failed / Request
	// was cancelled") with no status to match on, so the filter's own DENY is
	// what distinguishes a real denial from an unrelated failure.
	out, code = fetch(proxyTestDeniedHost)
	if code == 0 {
		t.Errorf("node fetch of non-allowlisted host succeeded (out=%q); the filter should deny it", out)
	}
	if v := dec.verdictFor(proxyTestDeniedHost); v != "DENY" {
		t.Errorf("filter verdict for %s = %q, want DENY (decisions: %v)", proxyTestDeniedHost, v, dec.all())
	}
}

// upstreamPort extracts the port from an httptest server URL.
func upstreamPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse upstream port from %q: %v", rawURL, err)
	}
	return port
}

// nodeMajor returns the major version of the node at path, or fails.
func nodeMajor(t *testing.T, node string) int {
	t.Helper()
	out, err := exec.Command(node, "-e", "process.stdout.write(String(process.versions.node.split('.')[0]))").Output()
	if err != nil {
		t.Fatalf("node version probe: %v", err)
	}
	major, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse node major %q: %v", out, err)
	}
	return major
}
