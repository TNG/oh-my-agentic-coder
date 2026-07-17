package sandboxrun

import (
	"fmt"
	"net/url"
	"strings"
)

// JVMProxyToolOptions renders a JAVA_TOOL_OPTIONS value that routes every
// JVM launched in the sandbox — Gradle, Maven, sbt, Kotlin, plain java —
// through proxyURL. The JVM (unlike curl/git/pip/go) ignores the
// HTTP(S)_PROXY environment variables, so under a filtered sandbox its
// direct connections are blocked by the kernel before the proxy filter
// runs: no repository is reachable and no allow/deny prompt can fire.
// Setting the proxy system properties via JAVA_TOOL_OPTIONS makes the JVM
// dial the omac proxy and forward the proxy Basic-auth credentials that
// the CONNECT tunnel requires (bare proxyHost/Port would 407).
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
			fmt.Sprintf("-D%s.nonProxyHosts=localhost|127.*|[::1]", scheme),
		)
		if user != "" {
			opts = append(opts,
				fmt.Sprintf("-D%s.proxyUser=%s", scheme, user),
				fmt.Sprintf("-D%s.proxyPassword=%s", scheme, pass),
			)
		}
	}
	// Java 8u111+ disables Basic auth on HTTPS CONNECT tunnels by
	// default; re-enable it so the proxy token is accepted.
	opts = append(opts, "-Djdk.http.auth.tunneling.disabledSchemes=")
	return strings.Join(opts, " "), nil
}
