//go:build vuln

package sandboxrun

import (
	"strings"
	"testing"
)

// TestSecurityWriteAllowsDoNotOverrideProtectedDenies asserts that a broad
// write grant covering a protected path does not let Seatbelt's
// last-match-wins semantics override that path's protected-write deny.
//
// GenerateSBPL emits, in order: read allows, then protected-path denies,
// then write allows. Seatbelt evaluates rules last-match-wins, so for a
// path that is both write-granted (e.g. via a broad $HOME allow) and
// protected (e.g. ~/.ssh), the write-allow — emitted after the deny —
// wins, silently overriding the protection for writes. Reads stay denied,
// since nothing write-related follows the read-allow section.
func TestSecurityWriteAllowsDoNotOverrideProtectedDenies(t *testing.T) {
	home := "/Users/u"
	ssh := home + "/.ssh"

	g := &Grants{
		Workdir:        home,
		AllowPaths:     []string{home},
		ProtectedPaths: []string{ssh},
		NetworkMode:    "blocked",
	}
	sbpl := GenerateSBPL(g)

	writeAllowIdx := strings.Index(sbpl, "(allow file-write* (subpath \""+home+"\"))")
	denyWriteIdx := strings.Index(sbpl, "(deny file-write* (subpath \""+ssh+"\"))")
	if writeAllowIdx < 0 || denyWriteIdx < 0 {
		t.Fatalf("control: expected rules not found in generated profile:\n%s", sbpl)
	}

	// Control: the read side is correctly ordered (deny after nothing
	// write-related follows it) — reads of a protected path under a broad
	// read grant stay denied. This isolates the assertion to the write
	// side specifically.
	readAllowIdx := strings.Index(sbpl, "(allow file-read* (subpath \""+home+"\"))")
	denyReadIdx := strings.Index(sbpl, "(deny file-read* (subpath \""+ssh+"\"))")
	if readAllowIdx < 0 || denyReadIdx < 0 || readAllowIdx > denyReadIdx {
		t.Fatalf("control: the read-side ordering is not as expected: the fixture is broken, not the security property")
	}

	if denyWriteIdx < writeAllowIdx {
		t.Errorf("(deny file-write* (subpath %q)) appears before (allow file-write* (subpath %q)) in the generated profile: "+
			"Seatbelt is last-match-wins, so the later write-allow overrides the protected-path deny and %s becomes writable", ssh, home, ssh)
	}
}
