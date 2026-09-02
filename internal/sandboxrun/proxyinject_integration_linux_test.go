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

	"github.com/TNG/oh-my-agentic-coder/internal/netproxy"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
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

// verdictFor returns the recorded verdict word ("ALLOW"/"DENY") and its reason
// for host, or ("", "") when the filter never ruled on it (i.e. the request
// never reached the proxy at all). Asserting the reason pins the outcome to a
// specific filter decision: a policy DENY for a non-allowlisted host is
// "not in allowlist", so an unrelated denial (auth failure, hard-deny) cannot
// be mistaken for the policy decision the test means to exercise.
func (d *proxyDecisions) verdictFor(host string) (string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.lines {
		if !strings.Contains(l, host+":") {
			continue
		}
		if strings.Contains(l, "net ALLOW") {
			return "ALLOW", extractReason(l)
		}
		if strings.Contains(l, "net DENY") {
			return "DENY", extractReason(l)
		}
	}
	return "", ""
}

// extractReason pulls the trailing "(reason)" out of a filter log line of the
// form "omac sandbox: net <VERDICT> <host>:<port> (<reason>)". Reasons never
// contain parentheses, so a last-( / last-) slice is sufficient.
func extractReason(line string) string {
	i := strings.LastIndex(line, "(")
	if i < 0 {
		return ""
	}
	j := strings.LastIndex(line, ")")
	if j < i {
		return ""
	}
	return line[i+1 : j]
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
	build := exec.Command("go", "build", "-o", omac, "github.com/TNG/oh-my-agentic-coder/cmd/omac")
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
// assumption. extraReads adds read grants beyond the tool's own (e.g. a
// precompiled class dir). Returns combined output and exit code.
func runToolThroughProxy(t *testing.T, omac string, proxy *netproxy.Server, injected map[string]string, extraReads []string, tool string, args ...string) (string, int) {
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
	// Landlock allows the proxy port (Stage2Args emits --connect-tcp for it).
	// The tool's own grants come from resolveInnerBinaryDirs — the same helper
	// BuildChildArgv uses — so this exercises production's notion of "what does
	// this binary need to be reachable" rather than a parallel guess that can
	// drift from it.
	g.ProxyPort = proxy.Port()
	g.ReadPaths = append(g.ReadPaths, filepath.Dir(omac))
	g.ReadPaths = append(g.ReadPaths, resolveInnerBinaryDirs([]string{tool})...)
	g.ReadPaths = append(g.ReadPaths, extraReads...)

	stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
	argvTail := append(append([]string{}, stage2...), "--", tool)
	argvTail = append(argvTail, args...)
	argv, err := BuildBwrapArgv(g, argvTail)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = sandboxprofile.FilterEnv(append(ambientPoison, os.Environ()...), nil, nil, injected)
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
		return runToolThroughProxy(t, omac, proxy, env, nil, curl,
			"-sS", "-k", "--max-time", "5",
			"-o", "/dev/null", "-w", "%{http_code}",
			fmt.Sprintf("https://%s:%d/", host, port))
	}

	out, code := get(proxyTestAllowedHost)
	if code != 0 || !strings.Contains(out, "200") {
		t.Errorf("allowlisted host: want HTTP 200 via proxy, got code=%d out=%q", code, out)
	}
	if v, reason := dec.verdictFor(proxyTestAllowedHost); v != "ALLOW" {
		t.Errorf("filter verdict for %s = %q (%q), want ALLOW (decisions: %v)", proxyTestAllowedHost, v, reason, dec.all())
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
	if v, reason := dec.verdictFor(proxyTestDeniedHost); v != "DENY" || reason != "not in allowlist" {
		t.Errorf("filter verdict for %s = %q (%q), want DENY (not in allowlist) (decisions: %v)", proxyTestDeniedHost, v, reason, dec.all())
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
		return runToolThroughProxy(t, omac, proxy, env, nil, node,
			"-e", script, fmt.Sprintf("https://%s:%d/", host, port))
	}

	out, code := fetch(proxyTestAllowedHost)
	if code != 0 || !strings.Contains(out, "STATUS 200") {
		t.Errorf("node fetch of allowlisted host: want STATUS 200 via proxy, got code=%d out=%q", code, out)
	}
	if v, reason := dec.verdictFor(proxyTestAllowedHost); v != "ALLOW" {
		t.Errorf("filter verdict for %s = %q (%q), want ALLOW (decisions: %v)", proxyTestAllowedHost, v, reason, dec.all())
	}

	// undici reports a denied CONNECT generically ("fetch failed / Request
	// was cancelled") with no status to match on, so the filter's own DENY is
	// what distinguishes a real denial from an unrelated failure.
	out, code = fetch(proxyTestDeniedHost)
	if code == 0 {
		t.Errorf("node fetch of non-allowlisted host succeeded (out=%q); the filter should deny it", out)
	}
	if v, reason := dec.verdictFor(proxyTestDeniedHost); v != "DENY" || reason != "not in allowlist" {
		t.Errorf("filter verdict for %s = %q (%q), want DENY (not in allowlist) (decisions: %v)", proxyTestDeniedHost, v, reason, dec.all())
	}
}

// TestEnvLayeringDropsBlocklistAndOverlays asserts what the integration tests
// only assume: ambientPoison's JAVA_TOOL_OPTIONS is dropped by the blocklist,
// and the poisoned HTTP(S)_PROXY vars lose to the injected overlay. A
// regression in dangerousEnvExact or the overlay ordering would pass the
// tool-based tests silently (no JVM runs in the curl/node cases); this test
// makes the env-layering claim a real assertion. Needs no bwrap/Landlock.
func TestEnvLayeringDropsBlocklistAndOverlays(t *testing.T) {
	proxy, _ := startHermeticProxy(t)
	injected := injectedEnv(t, proxy)

	env := sandboxprofile.FilterEnv(append(ambientPoison, os.Environ()...), nil, nil, injected)

	// Blocklist: JAVA_TOOL_OPTIONS is on dangerousEnvExact and must be absent.
	for _, kv := range env {
		if strings.HasPrefix(kv, "JAVA_TOOL_OPTIONS=") {
			t.Errorf("JAVA_TOOL_OPTIONS survived FilterEnv (blocklist regression): %q", kv)
		}
	}
	// Overlay: the injected HTTP_PROXY must win over the poison.
	wantProxy := proxy.ProxyURL()
	for _, kv := range env {
		if strings.HasPrefix(kv, "HTTP_PROXY=") && kv != "HTTP_PROXY="+wantProxy {
			t.Errorf("HTTP_PROXY = %q, want injected %q (overlay regression)", kv, "HTTP_PROXY="+wantProxy)
		}
	}
	// Sanity: the injected value is actually present (not dropped entirely).
	if !envHas(env, "HTTP_PROXY", wantProxy) {
		t.Errorf("injected HTTP_PROXY missing from filtered env; got %v", env)
	}
}

func envHas(env []string, key, val string) bool {
	prefix := key + "="
	for _, kv := range env {
		if kv == prefix+val {
			return true
		}
	}
	return false
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

// TestIntegrationJVMThroughOmacProxy proves the `jvm` proxy_injection family
// works end-to-end: the JVM ignores HTTP(S)_PROXY, so under a filtered
// sandbox it would dial direct and be blocked by Landlock. With the
// JAVA_TOOL_OPTIONS the `jvm` injector sets (https.proxyHost/Port pointing at
// the omac proxy), a real `java` routes its HTTPS request through the proxy,
// and the filter records ALLOW for the allowlisted host and DENY for a
// non-allowlisted one.
//
// The bare JDK ignores the proxyUser/proxyPassword system properties, so the
// probe installs the Authenticator that Gradle and Maven install from them —
// see jvmFetchSource. Without that the proxy answers 407 before the filter
// ever runs, and there is no verdict to assert.
func TestIntegrationJVMThroughOmacProxy(t *testing.T) {
	requireBwrap(t)
	requireLandlockNet(t)
	java := requireTool(t, "java", "the jvm proxy_injection family needs a JDK")
	javac := requireTool(t, "javac", "compiling the JVM fetch probe needs it")

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "allowlisted-ok")
	}))
	defer upstream.Close()
	port := upstreamPort(t, upstream.URL)

	omac := buildOmacBinary(t)
	proxy, dec := startHermeticProxy(t)
	env := injectedEnv(t, proxy, sandboxprofile.ProxyInjectJVM)

	// Compile the probe on the host (javac isn't granted inside the sandbox);
	// grant the sandbox read access to the class dir so `java -cp` can load it.
	classDir := t.TempDir()
	src := filepath.Join(classDir, "Fetch.java")
	if err := os.WriteFile(src, []byte(jvmFetchSource), 0o644); err != nil {
		t.Fatal(err)
	}
	compile := exec.Command(javac, "-d", classDir, src)
	if out, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("javac: %v\n%s", err, out)
	}

	// The class dir is not part of the JDK's own install tree, so it needs an
	// explicit read grant on top of the tool's resolved dirs.
	runJava := func(host string) (string, int) {
		return runToolThroughProxy(t, omac, proxy, env, []string{classDir}, java,
			"-cp", classDir, "Fetch", fmt.Sprintf("https://%s:%d/", host, port))
	}

	// Allowlisted host: the JVM tunnels through the proxy and reaches the
	// upstream. Landlock permits only the proxy's port, so the 200 cannot have
	// come from a direct dial.
	out, code := runJava(proxyTestAllowedHost)
	if code != 0 || !strings.Contains(out, "STATUS 200") {
		t.Errorf("jvm fetch of allowlisted host: want STATUS 200 via proxy, got code=%d out=%q", code, out)
	}
	if v, reason := dec.verdictFor(proxyTestAllowedHost); v != "ALLOW" {
		t.Errorf("filter verdict for %s = %q (%q), want ALLOW (decisions: %v)", proxyTestAllowedHost, v, reason, dec.all())
	}

	// Non-allowlisted host: the filter DENYs the CONNECT, so the JDK fails to
	// tunnel. The filter's DENY reason is the pin — a 407 or an unrelated
	// failure leaves a different reason, or none at all.
	out, code = runJava(proxyTestDeniedHost)
	if code == 0 {
		t.Errorf("jvm fetch of non-allowlisted host succeeded (out=%q); the filter should deny it", out)
	}
	if v, reason := dec.verdictFor(proxyTestDeniedHost); v != "DENY" || reason != "not in allowlist" {
		t.Errorf("filter verdict for %s = %q (%q), want DENY (not in allowlist) (decisions: %v)", proxyTestDeniedHost, v, reason, dec.all())
	}
}

// jvmFetchSource is a minimal Java HTTPS client whose proxy routing is governed
// entirely by JAVA_TOOL_OPTIONS (https.proxyHost/Port). It prints the HTTP
// status code or the exception message.
//
// It installs an Authenticator reading the proxyUser/proxyPassword system
// properties, because the bare JDK does not consult them — that is the
// documented limitation in proxyinject.go:JVMProxyToolOptions, and Gradle and
// Maven (the real consumers of the `jvm` family) do exactly this wiring.
// Without it the omac proxy rejects every CONNECT with 407 at the auth stage,
// which is *before* the filter runs (netproxy.Server.handle), so no ALLOW/DENY
// verdict would ever be recorded and the test could not observe the filtering
// it exists to prove.
//
// The trust-all manager accepts the loopback httptest server's self-signed
// cert, the same concession curl makes with -k and node with
// NODE_TLS_REJECT_UNAUTHORIZED=0.
const jvmFetchSource = `import java.net.*;
import java.io.*;
import java.security.cert.X509Certificate;
import javax.net.ssl.*;
public class Fetch {
  public static void main(String[] a) throws Exception {
    final String user = System.getProperty("https.proxyUser", "");
    final String pass = System.getProperty("https.proxyPassword", "");
    Authenticator.setDefault(new Authenticator() {
      protected PasswordAuthentication getPasswordAuthentication() {
        if (getRequestorType() != RequestorType.PROXY) return null;
        return new PasswordAuthentication(user, pass.toCharArray());
      }
    });
    TrustManager[] trustAll = new TrustManager[]{ new X509TrustManager() {
      public X509Certificate[] getAcceptedIssuers() { return new X509Certificate[0]; }
      public void checkClientTrusted(X509Certificate[] c, String t) {}
      public void checkServerTrusted(X509Certificate[] c, String t) {}
    }};
    SSLContext ctx = SSLContext.getInstance("TLS");
    ctx.init(null, trustAll, new java.security.SecureRandom());
    HttpsURLConnection.setDefaultSSLSocketFactory(ctx.getSocketFactory());
    HttpsURLConnection.setDefaultHostnameVerifier((h, s) -> true);
    try {
      URL u = new URL(a[0]);
      HttpsURLConnection c = (HttpsURLConnection) u.openConnection();
      c.setConnectTimeout(5000);
      c.setReadTimeout(5000);
      System.out.println("STATUS " + c.getResponseCode());
    } catch (Exception e) {
      System.out.println("ERR " + e.getMessage());
      System.exit(1);
    }
  }
}
`
