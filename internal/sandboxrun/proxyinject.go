package sandboxrun

import (
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// nodeMinProxyEnvMajor is the first Node major that honors
// NODE_USE_ENV_PROXY; older runtimes ignore it (no-op, not an error).
const nodeMinProxyEnvMajor = 24

// proxyInjector renders the environment that routes one proxy-unaware
// toolchain family through proxyURL (the omac filtering proxy).
// proxyURL is netproxy.Server.ProxyURL(): http://omac:<token>@127.0.0.1:<port>.
// The returned vars are overlaid onto the child environment; the proxy
// token already rides in the child's HTTP_PROXY, so nothing here is a new
// secret. A family whose input is malformed returns an error (fail-loud).
type proxyInjector func(proxyURL string) (map[string]string, error)

// proxyInjectors maps each proxy_injection family to its injector. Adding
// a new proxy-unaware toolchain is one entry here; the accepted family
// names are validated in sandboxprofile.ProxyInjectionTools, and the test
// TestProxyInjectorsCoverProfileTools asserts the two stay in sync.
var proxyInjectors = map[string]proxyInjector{
	sandboxprofile.ProxyInjectJVM:  jvmProxyInject,
	sandboxprofile.ProxyInjectNode: nodeProxyInject,
}

// ProxyInjectionEnv returns the merged environment that routes every
// configured proxy_injection family through proxyURL. Families are
// pre-validated by sandboxprofile.Profile.Validate, so an unknown one here
// is a programming error rather than user input.
func ProxyInjectionEnv(families []string, proxyURL string) (map[string]string, error) {
	out := map[string]string{}
	for _, family := range families {
		inj, ok := proxyInjectors[family]
		if !ok {
			return nil, fmt.Errorf("proxy_injection: no injector for %q", family)
		}
		env, err := inj(proxyURL)
		if err != nil {
			return nil, err
		}
		for k, v := range env {
			out[k] = v
		}
	}
	return out, nil
}

// jvmProxyInject routes the JVM toolchain (Gradle, Maven, sbt, Kotlin,
// plain java) through proxyURL. The JVM ignores HTTP(S)_PROXY, so under a
// filtered sandbox its direct connections are blocked by the kernel before
// the proxy filter runs — no repository is reachable and no allow/deny
// prompt can fire. A controlled JAVA_TOOL_OPTIONS points every JVM at the
// proxy instead.
func jvmProxyInject(proxyURL string) (map[string]string, error) {
	opts, err := JVMProxyToolOptions(proxyURL)
	if err != nil {
		return nil, err
	}
	return map[string]string{"JAVA_TOOL_OPTIONS": opts}, nil
}

// nodeProxyInject routes Node's built-in fetch/http (undici) through
// proxyURL. The package managers (npm, yarn, pnpm) already honor the
// injected HTTP(S)_PROXY env, but Node's runtime HTTP client ignores it
// unless opted in. NODE_USE_ENV_PROXY=1 makes Node route through the
// already-injected HTTP(S)_PROXY (with its userinfo token). Requires
// Node >= 24; older runtimes ignore the variable (no-op, not an error).
func nodeProxyInject(proxyURL string) (map[string]string, error) {
	if _, err := url.Parse(proxyURL); err != nil {
		return nil, fmt.Errorf("proxy_injection: parse proxy url: %w", err)
	}
	return map[string]string{"NODE_USE_ENV_PROXY": "1"}, nil
}

// JVMProxyToolOptions renders a JAVA_TOOL_OPTIONS value that points every
// JVM launched in the sandbox — Gradle, Maven, sbt, Kotlin, plain java —
// at proxyURL via the http(s).proxyHost/Port system properties.
//
// The proxyUser/proxyPassword properties carry the proxy's Basic-auth
// credentials, but only tools that parse them themselves (Gradle, Maven)
// authenticate with them. The core JDK HTTP clients (HttpURLConnection,
// java.net.http.HttpClient — e.g. a bare java) ignore these and would 407
// against the CONNECT tunnel; host/port routing still applies. omac targets
// the JVM build tools here — bare-JDK proxy auth is out of scope.
//
// proxyURL is netproxy.Server.ProxyURL (http://omac:<token>@127.0.0.1:<port>).
// The token already lives in the child's HTTP_PROXY variable, so this
// surfaces no new secret. Note: the JVM prints a one-line
// "Picked up JAVA_TOOL_OPTIONS: ..." notice (containing the token) to
// stderr on every launch; the token is ephemeral and proxy-scoped.
func JVMProxyToolOptions(proxyURL string) (string, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return "", fmt.Errorf("proxy_injection: parse proxy url: %w", err)
	}
	host := u.Hostname()
	port := u.Port()
	if host == "" || port == "" {
		return "", fmt.Errorf("proxy_injection: proxy url %q lacks host:port", proxyURL)
	}
	user := u.User.Username()
	pass, _ := u.User.Password()

	var opts []string
	for _, scheme := range []string{"http", "https"} {
		opts = append(opts,
			fmt.Sprintf("-D%s.proxyHost=%s", scheme, host),
			fmt.Sprintf("-D%s.proxyPort=%s", scheme, port),
		)
		if user != "" {
			opts = append(opts,
				fmt.Sprintf("-D%s.proxyUser=%s", scheme, user),
				fmt.Sprintf("-D%s.proxyPassword=%s", scheme, pass),
			)
		}
	}
	// The JDK has no https.nonProxyHosts; http.nonProxyHosts governs both
	// schemes, so set it once.
	opts = append(opts, "-Dhttp.nonProxyHosts=localhost|127.*|[::1]")
	// Java 8u111+ disables Basic auth on HTTPS CONNECT tunnels by
	// default; re-enable it so the proxy token is accepted.
	opts = append(opts, "-Djdk.http.auth.tunneling.disabledSchemes=")
	return strings.Join(opts, " "), nil
}

// nodeProxyEnvSupported reports whether the `node --version` output belongs
// to a runtime that honors NODE_USE_ENV_PROXY (Node >= 24). Unparseable
// output is treated as unsupported so callers warn rather than silently
// claim routing.
func nodeProxyEnvSupported(versionOutput string) bool {
	v := strings.TrimSpace(versionOutput)
	v = strings.TrimPrefix(v, "v")
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return false
	}
	return n >= nodeMinProxyEnvMajor
}

// detectNodeProxySupport probes the node binary on PATH and reports whether
// it honors NODE_USE_ENV_PROXY, plus a human-readable detail for the warning
// omac emits when routing cannot actually be provided.
func detectNodeProxySupport() (supported bool, detail string) {
	path, err := exec.LookPath("node")
	if err != nil {
		return false, "node not found on PATH"
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return false, fmt.Sprintf("could not run `node --version`: %v", err)
	}
	version := strings.TrimSpace(string(out))
	if !nodeProxyEnvSupported(version) {
		return false, fmt.Sprintf("found Node %s (need >= %d)", version, nodeMinProxyEnvMajor)
	}
	return true, version
}
