//go:build vuln

package sandboxrun

import (
	"bytes"
	"strings"
	"testing"
)

// TestSecurityLearnPromptStripsControlSequences asserts that a learn-mode
// candidate path containing terminal control sequences cannot repaint the
// operator's terminal or forge extra prompt lines.
//
// OfferLearnedFolders prints each candidate with a plain fmt.Fprintf, with
// no escaping — unlike the sibling intent prompt, which renders with %q.
func TestSecurityLearnPromptStripsControlSequences(t *testing.T) {
	var out bytes.Buffer
	payload := "innocent\x1b[2J\rFAKE — approved automatically"

	if err := OfferLearnedFolders("", []string{payload}, strings.NewReader("N\n"), &out, ""); err != nil {
		t.Fatalf("OfferLearnedFolders: %v", err)
	}

	rendered := out.String()

	// Control: the candidate is shown at all (declining doesn't suppress
	// the listing), so a fix that stops listing candidates altogether
	// wouldn't make this test pass for the wrong reason.
	if !strings.Contains(rendered, "innocent") {
		t.Fatalf("control: the candidate was not rendered at all (%q): the fixture is broken, not the security property", rendered)
	}

	if strings.ContainsRune(rendered, 0x1b) || strings.ContainsRune(rendered, '\r') {
		t.Errorf("learn-mode prompt output contains raw control bytes from a candidate path: %q", rendered)
	}
}
