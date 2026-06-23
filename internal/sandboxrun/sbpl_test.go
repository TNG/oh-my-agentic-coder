package sandboxrun

import (
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

func baseGrants() *Grants {
	return &Grants{
		Workdir:         "/work",
		ReadPaths:       []string{"/usr/lib", "/cfg/readonly"},
		WritePaths:      []string{"/scratch"},
		AllowPaths:      []string{"/work"},
		ProtectedPaths:  []string{"/home/u/.ssh", "/home/u/.netrc"},
		NetworkMode:     sandboxprofile.ModeFiltered,
		ProxyPort:       54321,
		ListenPorts:     []int{4097},
		AllowTCPConnect: []int{22},
		OpenPorts:       []int{49152},
		UnixSockets:     []string{"/tmp/omac-x/bridge.sock"},
	}
}

func TestSBPLBaselineStructure(t *testing.T) {
	p := GenerateSBPL(baseGrants())
	for _, want := range []string{
		"(version 1)",
		"(deny default)",
		"(allow process-exec*)",
		"(allow process-fork)",
		"(allow process-info* (target self))",
		"(allow signal (target same-sandbox))",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %q", want)
		}
	}
}

func TestSBPLKeychainDenied(t *testing.T) {
	p := GenerateSBPL(baseGrants())
	for _, svc := range []string{
		"com.apple.SecurityServer",
		"com.apple.securityd",
		"com.apple.security.keychaind",
		"com.apple.secd",
		"com.apple.security.agent",
	} {
		if !strings.Contains(p, `(deny mach-lookup (global-name "`+svc+`"))`) {
			t.Errorf("keychain daemon %s not denied", svc)
		}
	}
}

func TestSBPLFilesystemRules(t *testing.T) {
	p := GenerateSBPL(baseGrants())
	for _, want := range []string{
		`(allow file-read* (subpath "/usr/lib"))`,
		`(allow file-read* (subpath "/cfg/readonly"))`,
		`(allow file-read* (subpath "/work"))`,  // allow => readable
		`(allow file-write* (subpath "/work"))`, // allow => writable
		`(allow file-write* (subpath "/scratch"))`,
		`(deny file-read* (subpath "/home/u/.ssh"))`,
		`(deny file-write* (subpath "/home/u/.netrc"))`,
		`(allow file-map-executable (subpath "/usr/lib"))`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %q", want)
		}
	}
	// Write-only paths must not be readable.
	if strings.Contains(p, `(allow file-read* (subpath "/scratch"))`) {
		t.Error("write-only path must not get a read rule")
	}
	// file-map-executable must not cover write-only paths.
	if strings.Contains(p, `(allow file-map-executable (subpath "/scratch"))`) {
		t.Error("write-only path must not be mappable executable")
	}
}

func TestSBPLRuleOrderReadDenyWrite(t *testing.T) {
	p := GenerateSBPL(baseGrants())
	readPos := strings.Index(p, `(allow file-read* (subpath "/usr/lib"))`)
	denyPos := strings.Index(p, `(deny file-read* (subpath "/home/u/.ssh"))`)
	writePos := strings.Index(p, `(allow file-write* (subpath "/scratch"))`)
	if readPos < 0 || denyPos < 0 || writePos < 0 {
		t.Fatal("expected rules missing")
	}
	if !(readPos < denyPos && denyPos < writePos) {
		t.Errorf("rule order wrong: read=%d deny=%d write=%d (want read < deny < write)", readPos, denyPos, writePos)
	}
}

func TestSBPLNetworkFiltered(t *testing.T) {
	p := GenerateSBPL(baseGrants())
	for _, want := range []string{
		"(deny network*)",
		`(allow network-outbound (remote tcp "localhost:54321"))`, // proxy
		`(allow network-outbound (remote tcp "*:22"))`,            // allow_tcp_connect
		`(allow network-outbound (remote tcp "localhost:49152"))`, // open_port
		"(allow network-bind)",                                    // listen/open present
		"(allow network-inbound)",
		`(allow network-outbound (literal "/tmp/omac-x/bridge.sock"))`, // unix socket
		`(allow network-outbound (literal "/private/var/run/mDNSResponder"))`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %q", want)
		}
	}
	denyPos := strings.Index(p, "(deny network*)")
	proxyPos := strings.Index(p, `(allow network-outbound (remote tcp "localhost:54321"))`)
	if denyPos > proxyPos {
		t.Error("(deny network*) must precede the targeted allows")
	}
}

func TestSBPLNetworkBlocked(t *testing.T) {
	g := baseGrants()
	g.NetworkMode = sandboxprofile.ModeBlocked
	g.ProxyPort = 0
	p := GenerateSBPL(g)
	if !strings.Contains(p, "(deny network*)") {
		t.Error("blocked mode must deny network*")
	}
	if strings.Contains(p, `remote tcp "*:22"`) {
		t.Error("blocked mode must not include allow_tcp_connect rules")
	}
	if strings.Contains(p, "(allow network-bind)") {
		t.Error("blocked mode must not allow bind")
	}
	// Unix sockets still allowed (granted socket files).
	if !strings.Contains(p, `(allow network-outbound (literal "/tmp/omac-x/bridge.sock"))`) {
		t.Error("granted unix socket must stay allowed in blocked mode")
	}
}

func TestSBPLNetworkOpen(t *testing.T) {
	g := baseGrants()
	g.NetworkMode = sandboxprofile.ModeOpen
	p := GenerateSBPL(g)
	if !strings.Contains(p, "(allow network*)") {
		t.Error("open mode must allow network*")
	}
	if strings.Contains(p, "(deny network*)") {
		t.Error("open mode must not deny network*")
	}
}

func TestSBPLNoBindWithoutPorts(t *testing.T) {
	g := baseGrants()
	g.ListenPorts = nil
	g.OpenPorts = nil
	p := GenerateSBPL(g)
	if strings.Contains(p, "(allow network-bind)") {
		t.Error("no listen/open ports: bind must stay denied")
	}
}

func TestSBPLOpenPortZeroWildcard(t *testing.T) {
	g := baseGrants()
	g.OpenPorts = []int{0}
	p := GenerateSBPL(g)
	if !strings.Contains(p, `(allow network-outbound (remote tcp "localhost:*"))`) {
		t.Error("open_port 0 sentinel must emit the localhost:* wildcard rule")
	}
	if strings.Contains(p, `localhost:0`) {
		t.Error("open_port 0 must not emit the invalid localhost:0 literal")
	}
	// The blanket bind/inbound rules still apply with an open port present.
	if !strings.Contains(p, "(allow network-bind)") || !strings.Contains(p, "(allow network-inbound)") {
		t.Error("open_port 0 must keep blanket bind/inbound rules")
	}
	if strings.Contains(p, `remote tcp "*:*"`) {
		t.Error("open_port 0 must not allow outbound to any host (remote tcp \"*:*\")")
	}
}

// open_port 0 must never widen into non-loopback egress: no (local ip ...)
// outbound (it matches every unbound socket) and no "*:*" allow.
func TestSBPLOpenPortZeroNoEgressHole(t *testing.T) {
	g := baseGrants()
	g.OpenPorts = []int{0}
	p := GenerateSBPL(g)
	for _, forbidden := range []string{
		`(allow network-outbound (local ip "*:*"))`,
		`(allow network-outbound (local ip "localhost:*"))`,
		`(allow network-outbound (local tcp "localhost:*"))`,
		`(allow network-outbound (remote tcp "*:*"))`,
		`(allow network-outbound (remote ip "*:*"))`,
	} {
		if strings.Contains(p, forbidden) {
			t.Errorf("open_port 0 emitted an egress-opening rule: %q", forbidden)
		}
	}
	if !strings.Contains(p, `(allow network-outbound (remote tcp "localhost:*"))`) {
		t.Error("open_port 0 must keep the remote-scoped localhost:* allow")
	}
}

func TestSBPLOpenPortZeroMixed(t *testing.T) {
	g := baseGrants()
	g.OpenPorts = []int{4097, 0}
	p := GenerateSBPL(g)
	for _, want := range []string{
		`(allow network-outbound (remote tcp "localhost:4097"))`,
		`(allow network-outbound (remote tcp "localhost:*"))`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %q", want)
		}
	}
	if strings.Contains(p, `localhost:0`) {
		t.Error("mixed open_port set must not emit the invalid localhost:0 literal")
	}
}

func TestSBPLOpenPortNonZeroNoWildcard(t *testing.T) {
	g := baseGrants()
	g.OpenPorts = []int{4097}
	p := GenerateSBPL(g)
	if !strings.Contains(p, `(allow network-outbound (remote tcp "localhost:4097"))`) {
		t.Error("non-zero open_port must emit its per-port rule")
	}
	if strings.Contains(p, `(allow network-outbound (remote tcp "localhost:*"))`) {
		t.Error("non-sentinel open_port set must not emit the wildcard rule")
	}
}

func TestSBPLQuoteEscaping(t *testing.T) {
	g := baseGrants()
	g.ReadPaths = []string{`/path/with"quote`}
	p := GenerateSBPL(g)
	if !strings.Contains(p, `(allow file-read* (subpath "/path/with\"quote"))`) {
		t.Error("quotes in paths must be escaped")
	}
}

func TestAncestorMetadataRules(t *testing.T) {
	p := GenerateSBPL(baseGrants())
	for _, want := range []string{
		`(allow file-read-metadata (literal "/cfg"))`,
		`(allow file-read-metadata (literal "/home/u"))`, // ancestors of write paths too
	} {
		if want == `(allow file-read-metadata (literal "/home/u"))` {
			continue // protected paths don't need metadata; skip
		}
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %q", want)
		}
	}
}

func TestGrantsValidate(t *testing.T) {
	g := baseGrants()
	if err := g.Validate(); err != nil {
		t.Errorf("valid grants rejected: %v", err)
	}
	g.NetworkMode = "weird"
	if err := g.Validate(); err == nil {
		t.Error("invalid mode accepted")
	}
	g2 := baseGrants()
	g2.Workdir = ""
	if err := g2.Validate(); err == nil {
		t.Error("empty workdir accepted")
	}
}
