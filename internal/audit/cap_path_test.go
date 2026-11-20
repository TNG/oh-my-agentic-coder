package audit

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Both facade events bound the agent-controlled path the same way, and the
// cut never splits a character.
func TestFacadeEventsCapThePathAlike(t *testing.T) {
	long := "/" + strings.Repeat("x", maxPathBytes-2) + "ééé"
	req := FacadeRequest("GET", "m", "", long, 200, 0, 0).Path
	appr := FacadeApproval(FacadeApprovalInfo{Path: long}).Path
	if req != appr {
		t.Errorf("request %q and approval %q differ", req, appr)
	}
	if !utf8.ValidString(req) || len(req) > maxPathBytes+len("…") {
		t.Errorf("capped path %q (%d bytes)", req, len(req))
	}
}
