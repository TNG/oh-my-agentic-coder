package netproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// closedPort returns an address nothing listens on, so a dial to it is
// refused — the shape of an allowed-but-unreachable (VPN-down) registry.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// TestServer502BodyCarriesRegistryHint pins the connection-failure hint to the
// *response*, not the log. Diagnostics go to ~/.local/state/omac/sandbox.log
// whenever stderr is a terminal, so a log-only hint is invisible to the agent
// that made the request and to the user until they open that file. The client
// sees the 502 body, so the explanation has to be in it.
func TestServer502BodyCarriesRegistryHint(t *testing.T) {
	body := connectToUnreachable(t, "registry.corp.example.com")
	if !strings.Contains(body, "package registry") {
		t.Errorf("502 body should name the registry and explain the failure;\ngot:\n%s", body)
	}
	if !strings.Contains(body, "VPN") {
		t.Errorf("502 body should point at VPN/reachability;\ngot:\n%s", body)
	}
}

// TestServer502BodyWrapWidth holds the 502 body to the same wrap discipline as
// the deny body: it is read in a terminal, so the appended hint must not arrive
// as one runaway line.
func TestServer502BodyWrapWidth(t *testing.T) {
	const maxWidth = 100
	for _, line := range strings.Split(connectToUnreachable(t, "registry.corp.example.com"), "\n") {
		if len(line) > maxWidth {
			t.Errorf("502 body line of %d chars exceeds %d:\n%s", len(line), maxWidth, line)
		}
	}
}

// TestRegistryUpstreamHintLogsOneLine guards the diagnostics contract: the diag
// sink appends one line per entry so concurrent sandboxes interleave whole
// lines, and it prepends timestamp/level/category columns to each. The hint is
// wrapped prose for the response body, so the logged form must be flattened.
func TestRegistryUpstreamHintLogsOneLine(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	filter := NewFilter(FilterConfig{
		AllowDomains: []string{"registry.corp.example.com"},
		Resolve:      resolveTo("127.0.0.1"),
	})
	s, err := NewServer(filter, NewDirectDialer(), func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	port := closedPort(t)
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	target := fmt.Sprintf("registry.corp.example.com:%d", port)
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n",
		target, target, basicAuth("omac", s.Token()))
	if _, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	var found bool
	for _, msg := range logged {
		if !strings.Contains(msg, "package registry") {
			continue
		}
		found = true
		if strings.Contains(strings.TrimSuffix(msg, "\n"), "\n") {
			t.Errorf("registry hint must be logged as a single line;\ngot:\n%s", msg)
		}
	}
	if !found {
		t.Errorf("registry hint should still reach the log; got %q", logged)
	}
}

// connectToUnreachable CONNECTs through the proxy to host on a port nothing
// listens on — host is allowed by policy, so the failure is the dial, not the
// filter. Returns the body the client receives.
func connectToUnreachable(t *testing.T, host string) string {
	t.Helper()
	port := closedPort(t)
	s := startProxy(t, FilterConfig{
		AllowDomains: []string{host},
		Resolve:      resolveTo("127.0.0.1"),
	})

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	target := fmt.Sprintf("%s:%d", host, port)
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n",
		target, target, basicAuth("omac", s.Token()))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestIsPackageRegistry(t *testing.T) {
	for _, tt := range []struct {
		host string
		want bool
	}{
		{"registry.npmjs.org", true},
		{"registry.npmjs.org.", true}, // trailing dot
		{"files.pythonhosted.org", true},
		{"crates.io", true},
		{"proxy.golang.org", true},
		{"npm.pkg.github.com", true},
		{"registry.corp.example.com", true}, // private, "registry" label
		{"npm.internal.example", true},      // private, "npm" label
		{"api.anthropic.com", false},
		{"models.dev", false},
		{"github.com", false},
		{"example.com", false},
	} {
		if got := isPackageRegistry(tt.host); got != tt.want {
			t.Errorf("isPackageRegistry(%q) = %v; want %v", tt.host, got, tt.want)
		}
	}
}

func TestRegistryDenyHintFiresOnlyForRegistries(t *testing.T) {
	if got := registryDenyHint("api.anthropic.com", "not in allowlist"); got != "" {
		t.Errorf("hint should be empty for a non-registry host; got %q", got)
	}
	if got := registryDenyHint("registry.npmjs.org", "not in allowlist"); got == "" {
		t.Fatal("hint should be non-empty for a registry host")
	}
}

// TestRegistryDenyHintTailorsByReason checks the remedy matches why the request
// was denied — the distinction that tells a user whether a prompt could even
// have been shown.
func TestRegistryDenyHintTailorsByReason(t *testing.T) {
	for _, tt := range []struct {
		reason string
		want   string
	}{
		{"prompt unavailable: on_unavailable=deny", "no interactive network prompt"},
		{"not in allowlist", "network prompt is disabled"},
		{"prompt:deny", "denied at the network prompt"},
		{"deny_domain", "matches a deny rule"},
	} {
		got := registryDenyHint("registry.npmjs.org", tt.reason)
		if !strings.Contains(got, tt.want) {
			t.Errorf("reason %q: hint missing %q;\ngot:\n%s", tt.reason, tt.want, got)
		}
	}
}

// TestRegistryDenyHintWrapWidth pins the rendered shape: the hint is read as
// plain text in a terminal alongside a body wrapped at ~80 columns, so no line
// may run away. Every reason branch is checked, because the remedy text is
// interpolated into the shared preamble and a bad splice point ruins all of
// them at once.
func TestRegistryDenyHintWrapWidth(t *testing.T) {
	const maxWidth = 100
	for _, reason := range []string{
		"prompt unavailable: on_unavailable=deny",
		"not in allowlist",
		"prompt:deny",
		"prompt:needs_intent",
		"dns resolution failed",
		"hard-deny: resolves to link-local",
		"deny_domain",
		"learned permanent deny",
	} {
		for _, line := range strings.Split(registryDenyHint("registry.npmjs.org", reason), "\n") {
			if len(line) > maxWidth {
				t.Errorf("reason %q: line of %d chars exceeds %d:\n%s",
					reason, len(line), maxWidth, line)
			}
		}
	}
}

// TestDenyBodyNeedsIntentCarriesRegistryHint covers the "Explain more" path:
// the user asked why the fetch is needed, and the agent has to answer. Naming
// the registry is exactly the context that makes a useful intent, and the
// remedy is the intent round-trip — not a deny rule to remove.
func TestDenyBodyNeedsIntentCarriesRegistryHint(t *testing.T) {
	body := denyBody("registry.npmjs.org", Verdict{Reason: "prompt:needs_intent"})
	if !strings.Contains(body, "package registry") {
		t.Errorf("needs_intent body should carry the registry note;\ngot:\n%s", body)
	}
	if strings.Contains(body, "deny rule") || strings.Contains(body, "deny_domain") {
		t.Errorf("needs_intent body must not blame a deny rule;\ngot:\n%s", body)
	}
	if !strings.Contains(body, "/sandbox/intent") {
		t.Errorf("needs_intent body must keep the intent round-trip;\ngot:\n%s", body)
	}
}

// TestDenyBodyDNSFailureDoesNotClaimPolicy fixes a body that contradicted
// itself: a name that fails to resolve is a Deny verdict, so it took the policy
// text — "DENIED BY THE SANDBOX network policy", followed by three policy knobs
// to edit — while the registry hint below it correctly said nothing was denied
// by policy. Editing allow_domain cannot make a name resolve.
func TestDenyBodyDNSFailureDoesNotClaimPolicy(t *testing.T) {
	body := denyBody("registry.corp.example.com", Verdict{Reason: "dns resolution failed"})
	if strings.Contains(body, "DENIED BY THE SANDBOX network policy") {
		t.Errorf("resolution failure is not a policy denial;\ngot:\n%s", body)
	}
	if strings.Contains(body, "allow_domain") || strings.Contains(body, "deny_domain") {
		t.Errorf("body must not offer policy knobs as the remedy;\ngot:\n%s", body)
	}
	if !strings.Contains(body, "resolve") {
		t.Errorf("body should say the name did not resolve;\ngot:\n%s", body)
	}
}

// TestDenyBodyHardDenyDoesNotOfferAllowlist covers the built-in guards: metadata
// hostnames and link-local addresses are checked before any rule, so
// allow_domain cannot override them (verified against Filter.Check). Offering it
// as the remedy sends the user to edit a file that will not change the outcome.
func TestDenyBodyHardDenyDoesNotOfferAllowlist(t *testing.T) {
	for _, reason := range []string{
		"hard-deny metadata host",
		"hard-deny link-local address",
		"hard-deny: resolves to link-local",
	} {
		body := denyBody("metadata.google.internal", Verdict{Reason: reason})
		if strings.Contains(body, "allow_domain") {
			t.Errorf("reason %q: allow_domain cannot override a hard-deny;\ngot:\n%s", reason, body)
		}
		if !strings.Contains(body, "cannot be overridden") {
			t.Errorf("reason %q: body should say the guard is not overridable;\ngot:\n%s", reason, body)
		}
	}
}

// TestDenyBodyDeclinedIntentOmitsRegistryHint resolves a conflict between two
// correct behaviours that met in the merge with #157. When the user has
// reviewed a declared intent and declined it, the intent hint says: do not
// retry. The registry hint's prompt:deny remedy says: re-run and choose Allow.
// On this path the decision has already been made on full information, so the
// registry note is withheld — offering install guidance would invite the loop
// the intent hint exists to close. A plain prompt:deny with nothing on file is
// unaffected and keeps its remedy.
func TestDenyBodyDeclinedIntentOmitsRegistryHint(t *testing.T) {
	declined := denyBody("registry.npmjs.org", Verdict{
		Reason:       "prompt:deny",
		IntentReason: "install the @tngtech opencode plugin",
	})
	if strings.Contains(declined, "package registry") {
		t.Errorf("declined-intent body must not carry install guidance;\ngot:\n%s", declined)
	}
	if !strings.Contains(declined, "declined it") {
		t.Errorf("declined-intent body should say the user declined;\ngot:\n%s", declined)
	}

	undeclared := denyBody("registry.npmjs.org", Verdict{Reason: "prompt:deny"})
	if !strings.Contains(undeclared, "package registry") {
		t.Errorf("plain prompt:deny should still carry the registry note;\ngot:\n%s", undeclared)
	}
}

// TestHardDenySurvivesAllowlist grounds the claim the hard-deny body makes: the
// guard is checked before any rule, so listing it in allow_domain changes
// nothing. Without this, the body's "cannot be overridden" wording rests on
// reading Filter.Check rather than on its behavior.
func TestHardDenySurvivesAllowlist(t *testing.T) {
	for _, host := range []string{"metadata.google.internal", "169.254.169.254"} {
		f := NewFilter(FilterConfig{
			AllowDomains: []string{host},
			Resolve:      resolveTo("127.0.0.1"),
		})
		v, _ := f.Check(context.Background(), host, 443)
		if v.Decision != Deny {
			t.Errorf("%q in allow_domain: decision = %v, want Deny", host, v.Decision)
		}
		if !strings.HasPrefix(v.Reason, "hard-deny") {
			t.Errorf("%q in allow_domain: reason = %q, want a hard-deny", host, v.Reason)
		}
	}
}

// TestDenyBodySessionDenyNamesTheRealRemedy: a session deny lives only
// in memory, so the generic policy body — which points at
// network.deny_domain and <profile>.pages.json — sends the agent hunting
// through files that cannot contain the entry. checkRules also
// short-circuits before the prompt, so the retry is denied with no
// dialog: the body must say the decision holds until the session ends.
func TestDenyBodySessionDenyNamesTheRealRemedy(t *testing.T) {
	body := denyBody("registry.npmjs.org", Verdict{Decision: Deny, Reason: "session deny"})
	if !strings.Contains(body, "session") {
		t.Errorf("body never mentions the session scope;\ngot:\n%s", body)
	}
	for _, wrong := range []string{"deny_domain", "pages.json"} {
		if strings.Contains(body, wrong) {
			t.Errorf("body points at %q, which never holds a session decision;\ngot:\n%s", wrong, body)
		}
	}
}

// TestDenyBodyKeepsPolicyTextForPolicyDenials is the counterweight to the two
// tests above: real policy denials must keep attributing the block to the
// sandbox and naming the knobs that change it.
func TestDenyBodyKeepsPolicyTextForPolicyDenials(t *testing.T) {
	for _, reason := range []string{
		"prompt unavailable: on_unavailable=deny",
		"not in allowlist",
		"prompt:deny",
		"deny_domain",
		"learned permanent deny",
	} {
		body := denyBody("registry.npmjs.org", Verdict{Reason: reason})
		if !strings.Contains(body, "DENIED BY THE SANDBOX network policy") {
			t.Errorf("reason %q: should attribute the denial to the sandbox;\ngot:\n%s", reason, body)
		}
		if !strings.Contains(body, "allow_domain") {
			t.Errorf("reason %q: should name allow_domain;\ngot:\n%s", reason, body)
		}
	}
}

// TestRegistryDenyHintDNSFailure guards the reachability case: a private/VPN
// registry that fails DNS resolution is a Deny verdict, so it flows through the
// deny body — but the remedy is reachability, not a deny rule. The hint must not
// misdirect the user to edit deny_domain.
func TestRegistryDenyHintDNSFailure(t *testing.T) {
	got := registryDenyHint("registry.corp.example.com", "dns resolution failed")
	if strings.Contains(got, "deny rule") || strings.Contains(got, "deny_domain") {
		t.Errorf("hint wrongly blames a deny rule;\ngot:\n%s", got)
	}
	if !strings.Contains(got, "VPN") && !strings.Contains(got, "reachable") {
		t.Errorf("hint should point at reachability/VPN;\ngot:\n%s", got)
	}
}

// TestRegistryDenyHintHardDeny guards the SSRF case: a registry host that
// resolves to a link-local/internal address is hard-denied. That is neither a
// deny rule nor a reachability problem, so the hint must not blame deny_domain.
func TestRegistryDenyHintHardDeny(t *testing.T) {
	got := registryDenyHint("registry.corp.example.com", "hard-deny: resolves to link-local")
	if strings.Contains(got, "deny rule") || strings.Contains(got, "deny_domain") {
		t.Errorf("hint wrongly blames a deny rule;\ngot:\n%s", got)
	}
	if !strings.Contains(got, "link-local") {
		t.Errorf("hint should name the link-local/internal-address block;\ngot:\n%s", got)
	}
}

func TestRegistryUpstreamHintFiresOnlyForRegistries(t *testing.T) {
	if got := registryUpstreamHint("github.com"); got != "" {
		t.Errorf("upstream hint should be empty for a non-registry host; got %q", got)
	}
	got := registryUpstreamHint("registry.npmjs.org")
	if !strings.Contains(got, "VPN") {
		t.Errorf("upstream hint should mention VPN/reachability;\ngot:\n%s", got)
	}
}

// TestDenyBodyAppendsRegistryHint confirms the hint reaches the body the client
// receives on a policy denial (the elegant single hook point).
func TestDenyBodyAppendsRegistryHint(t *testing.T) {
	body := denyBody("registry.npmjs.org", Verdict{Reason: "prompt unavailable: on_unavailable=deny"})
	if !strings.Contains(body, "looks like a package registry") {
		t.Errorf("denyBody should append the registry hint;\ngot:\n%s", body)
	}
	plain := denyBody("api.anthropic.com", Verdict{Reason: "prompt unavailable: on_unavailable=deny"})
	if strings.Contains(plain, "looks like a package registry") {
		t.Errorf("denyBody should not append the registry hint for a non-registry host;\ngot:\n%s", plain)
	}
}
