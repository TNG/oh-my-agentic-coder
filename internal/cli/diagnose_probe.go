// Package cli diagnose probe: `omac diagnose --probe host[:port]` static
// reachability checks (proxy allow_domain, raw-TCP allow_tcp_connect, loopback
// open_port). Split from diagnose.go so the retrospective audit analysis and
// the probe machinery can be reviewed independently.
package cli

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/netprompt"
	"github.com/tngtech/oh-my-agentic-coder/internal/netproxy"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// runDiagnoseProbe statically evaluates whether host[:port] would be
// admitted, reusing the real netproxy filter / profile rules (no DNS, no
// dialing, no sandbox launch) so the verdict cannot drift from runtime
// behavior.
func runDiagnoseProbe(env *Env, profile *sandboxprofile.Profile, profPath, target string, asJSON bool) int {
	host, port, err := splitHostPortDefault(target, 443)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac diagnose: --probe:", err)
		return ExitMisuse
	}

	var outcome probeOutcome
	var reason string
	loopback := isLoopbackHost(host)
	if loopback {
		// Loopback reachability is kernel-enforced by network.open_port, not
		// the proxy/domain filter — and a kernel denial leaves no trace in
		// the audit trail, so this static check is the only way to diagnose
		// it (e.g. a sandboxed tool reaching a local Ollama/DB, or the bridge
		// port). It is independent of allow_domain entirely.
		outcome, reason = loopbackProbe(profile, port)
	} else {
		learned, _ := netprompt.LoadLearnedPolicy(sandboxprofile.PagesPath(profPath))
		// PromptEnabled is false here so the filter evaluates the static rules
		// only and never blocks on an interactive prompt; the "would prompt"
		// outcome is derived afterwards from the profile's real setting.
		f := netproxy.NewFilter(netproxy.FilterConfig{
			AllowDomains:  profile.Network.AllowDomain,
			DenyDomains:   profile.Network.DenyDomain,
			PromptEnabled: false,
			Learned:       learned,
		})
		outcome, reason = classifyProbe(f.CheckHost(host, port), profile.Network.PromptEnabled())
	}

	pv := probeView{Host: host, Port: port, Outcome: string(outcome), Reason: reason}
	if !loopback {
		// A remote host has two independent paths: HTTP(S) via the proxy
		// (allow_domain, above) and raw TCP (SSH/git/DB), which bypasses the
		// proxy and needs the port in allow_tcp_connect. Report both so a
		// user debugging e.g. `git@github.com` (SSH:22) sees the right one.
		ro, rr := rawTCPProbe(profile, port)
		pv.RawTCP = &rawTCPView{Outcome: string(ro), Reason: rr}
	}

	if asJSON {
		return writeDiagnoseJSON(env, pv)
	}
	if loopback {
		fmt.Fprintf(env.Stdout, "%s %s:%d (%s)\n", outcome, host, port, reason)
	} else {
		fmt.Fprintf(env.Stdout, "%s:%d\n", host, port)
		fmt.Fprintf(env.Stdout, "  HTTP(S) via proxy: %-6s (%s)\n", outcome, reason)
		fmt.Fprintf(env.Stdout, "  raw TCP (SSH/DB):  %-6s (%s)\n", pv.RawTCP.Outcome, pv.RawTCP.Reason)
	}
	return ExitOK
}

// rawTCPProbe reports whether a direct (non-proxied) TCP connection to port
// would be admitted — the path SSH, git@…, and database clients use. It is
// gated by network.allow_tcp_connect (kernel-enforced, host-independent).
func rawTCPProbe(profile *sandboxprofile.Profile, port int) (probeOutcome, string) {
	for _, p := range profile.Network.AllowTCPConnect {
		if p == port {
			return probeAllow, fmt.Sprintf("port %d is in network.allow_tcp_connect", port)
		}
	}
	return probeDeny, fmt.Sprintf("port %d is not in network.allow_tcp_connect — add it only if a raw-TCP tool needs this port", port)
}

// isLoopbackHost reports whether target names the local host, whose
// reachability is governed by network.open_port rather than the proxy.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// loopbackProbe returns the verdict for a localhost:port connection: allowed
// iff the port is listed in network.open_port (which grants localhost TCP
// connect+bind), otherwise denied with the concrete fix.
func loopbackProbe(profile *sandboxprofile.Profile, port int) (probeOutcome, string) {
	for _, p := range profile.Network.OpenPort {
		if p == port {
			return probeAllow, "loopback port is in network.open_port"
		}
	}
	return probeDeny, fmt.Sprintf("loopback port %d is not in network.open_port — if the sandboxed tool needs this local service, add just this port; otherwise leave it closed", port)
}

type probeOutcome string

const (
	probeAllow  probeOutcome = "ALLOW"
	probeDeny   probeOutcome = "DENY"
	probePrompt probeOutcome = "PROMPT"
)

// classifyProbe turns a static filter verdict into the outcome a real run
// would produce. A verdict from an explicit rule (hard-deny/learned/
// deny_domain/allow_domain) is definitive; otherwise the request falls to
// the default path, where an enabled network prompt would ask the user
// interactively at runtime rather than silently allow/deny.
func classifyProbe(v netproxy.Verdict, promptEnabled bool) (probeOutcome, string) {
	definitive := strings.HasPrefix(v.Reason, "hard-deny") ||
		strings.HasPrefix(v.Reason, "learned") ||
		v.Reason == "deny_domain" || v.Reason == "allow_domain"
	if !definitive && promptEnabled {
		return probePrompt, "no static rule matches; the network prompt would ask interactively at runtime"
	}
	if v.Decision == netproxy.Allow {
		return probeAllow, v.Reason
	}
	return probeDeny, v.Reason
}

type probeView struct {
	Host    string      `json:"host"`
	Port    int         `json:"port"`
	Outcome string      `json:"outcome"`
	Reason  string      `json:"reason"`
	RawTCP  *rawTCPView `json:"raw_tcp,omitempty"`
}

// rawTCPView is the direct-TCP (allow_tcp_connect) verdict for a remote probe,
// alongside the proxy verdict in the top-level probeView fields.
type rawTCPView struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
}

func splitHostPortDefault(target string, defPort int) (string, int, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		// No port present: treat the whole string as the host.
		return target, defPort, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, port, nil
}
