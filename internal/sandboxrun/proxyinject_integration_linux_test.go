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
// netproxy/server_test.go). No real network, no LLM, no token; runs on the
// PR `go test ./...` gate.

const (
	proxyTestAllowedHost = "repo.omac-e2e.test"
	proxyTestDeniedHost  = "blocked.omac-e2e.test"
)

// startHermeticProxy builds an omac filtering proxy that allows exactly
// proxyTestAllowedHost and resolves every hostname to 127.0.0.1, so a
// loopback httptest upstream stands in for the allowlisted repository.
func startHermeticProxy(t *testing.T) *netproxy.Server {
	t.Helper()
	filter := netproxy.NewFilter(netproxy.FilterConfig{
		AllowDomains: []string{proxyTestAllowedHost},
		Resolve: func(_ context.Context, _ string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	})
	srv, err := netproxy.NewServer(filter, netproxy.NewDirectDialer(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
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

// runToolThroughProxy runs tool+args inside a filtered bwrap sandbox whose
// only permitted egress is proxy's loopback port, with extraEnv (the proxy
// and injection vars) overlaid on the child environment. Returns combined
// output and exit code.
func runToolThroughProxy(t *testing.T, omac string, proxy *netproxy.Server, extraEnv []string, tool string, args ...string) (string, int) {
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
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("exec: %v (%s)", err, out)
	}
	return string(out), 0
}

func proxyEnvSlice(proxy *netproxy.Server, extra map[string]string) []string {
	merged := proxy.EnvVars()
	for k, v := range extra {
		merged[k] = v
	}
	env := make([]string, 0, len(merged))
	for k, v := range merged {
		env = append(env, k+"="+v)
	}
	return env
}

// TestIntegrationCurlThroughOmacProxy proves a proxy-aware tool (curl,
// which honors HTTP(S)_PROXY) reaches an allowlisted host through the omac
// proxy under a filtered sandbox, and that a non-allowlisted host is denied
// by the filter (403 on CONNECT) rather than silently reachable.
func TestIntegrationCurlThroughOmacProxy(t *testing.T) {
	requireBwrap(t)
	if !LandlockNetSupported() {
		t.Skipf("Landlock ABI %d < 4", LandlockABI())
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not installed")
	}

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "allowlisted-ok")
	}))
	defer upstream.Close()
	port := upstreamPort(t, upstream.URL)

	omac := buildOmacBinary(t)
	proxy := startHermeticProxy(t)
	env := proxyEnvSlice(proxy, nil)

	get := func(host string) (string, int) {
		return runToolThroughProxy(t, omac, proxy, env, curl,
			"-sS", "-k", "--max-time", "5",
			"-o", "/dev/null", "-w", "%{http_code}",
			fmt.Sprintf("https://%s:%d/", host, port))
	}

	if out, code := get(proxyTestAllowedHost); code != 0 || !strings.Contains(out, "200") {
		t.Errorf("allowlisted host: want HTTP 200 via proxy, got code=%d out=%q", code, out)
	}
	if out, code := get(proxyTestDeniedHost); code == 0 && strings.Contains(out, "200") {
		t.Errorf("non-allowlisted host reached upstream (out=%q); the filter should deny it", out)
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
	if !LandlockNetSupported() {
		t.Skipf("Landlock ABI %d < 4", LandlockABI())
	}
	node, err := exec.LookPath("node")
	if err != nil {
		skipOrFailCI(t, "node not installed (the node proxy_injection family needs Node >= 24)")
	}
	if major := nodeMajor(t, node); major < 24 {
		skipOrFailCI(t, "node %d < 24: NODE_USE_ENV_PROXY is a no-op, cannot exercise the node family", major)
	}

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "allowlisted-ok")
	}))
	defer upstream.Close()
	port := upstreamPort(t, upstream.URL)

	omac := buildOmacBinary(t)
	proxy := startHermeticProxy(t)

	// NODE_USE_ENV_PROXY is exactly what nodeProxyInject injects; assert on
	// the same mechanism the profile wires up. NODE_TLS_REJECT_UNAUTHORIZED
	// accepts the httptest self-signed cert (curl uses -k for the same).
	inj, err := ProxyInjectionEnv([]string{sandboxprofile.ProxyInjectNode}, proxy.ProxyURL())
	if err != nil {
		t.Fatal(err)
	}
	inj["NODE_TLS_REJECT_UNAUTHORIZED"] = "0"
	env := proxyEnvSlice(proxy, inj)

	script := `const u=process.argv[1];` +
		`fetch(u).then(async r=>{console.log('STATUS',r.status,await r.text());process.exit(0)})` +
		`.catch(e=>{console.error('ERR',e.message);process.exit(1)});`

	fetch := func(host string) (string, int) {
		return runToolThroughProxy(t, omac, proxy, env, node,
			"-e", script, fmt.Sprintf("https://%s:%d/", host, port))
	}

	if out, code := fetch(proxyTestAllowedHost); code != 0 || !strings.Contains(out, "STATUS 200") {
		t.Errorf("node fetch of allowlisted host: want STATUS 200 via proxy, got code=%d out=%q", code, out)
	}
	if out, code := fetch(proxyTestDeniedHost); code == 0 && strings.Contains(out, "STATUS 200") {
		t.Errorf("node fetch reached non-allowlisted host (out=%q); the filter should deny it", out)
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
