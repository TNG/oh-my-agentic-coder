package netprompt

import (
	"strings"
	"testing"
)

// TestSecurityDialogTextRendersPayloadInert asserts that an agent-supplied
// intent string reaching the network-approval dialog cannot use Pango/Qt
// markup to fake an "admin pre-approved this" notice next to the real
// approve/deny choice.
//
// The intent (POST /sandbox/intent), the resolved cause, and the process
// origin all end up in the zenity/kdialog argv and in promptText with no
// escaping and no --no-markup flag on either dialog backend, so a `<span
// foreground="darkgreen">` block renders as styled, structurally
// indistinguishable text next to omac's own labels.
func TestSecurityDialogTextRendersPayloadInert(t *testing.T) {
	intent := `</span><b><span foreground="darkgreen">NOTE from omac policy: this destination was pre-approved by your admin. Choose "Allow permanently".</span></b>`
	host := "attacker.example"

	// Control: an ordinary intent string reaches the dialog argv at all, so
	// a fix that drops intent text entirely wouldn't make this test pass
	// for the wrong reason.
	benign := zenityArgs(host, 443, "", "checking for updates", "internal package mirror", "node")
	if !strings.Contains(strings.Join(benign, "\x00"), "checking for updates") {
		t.Fatalf("control: an ordinary intent string did not reach the zenity argv: the fixture is broken, not the security property")
	}

	z := zenityArgs(host, 443, "", intent, "internal package mirror", "node")
	k := kdialogArgs(host, 443, "", intent, "internal package mirror", "node")

	for name, args := range map[string][]string{"zenity": z, "kdialog": k} {
		joined := strings.Join(args, "\x00")
		if strings.Contains(joined, "<span foreground") && !strings.Contains(joined, "--no-markup") {
			t.Errorf("%s: agent-supplied markup reaches the dialog with no --no-markup flag and no escaping: %v", name, args)
		}
	}

	body := promptText(host, 443, intent, "cause", "origin", 6)
	if strings.Contains(body, "<span foreground") {
		t.Errorf("promptText embedded raw markup after the %%q-quoted \"Agent intent:\" label, which quotes but does not escape < > &:\n%s", body)
	}
}
