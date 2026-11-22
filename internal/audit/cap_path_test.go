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

func TestToolFieldsAreCapped(t *testing.T) {
	long := strings.Repeat("é", maxToolArgsBytes)
	for _, ev := range []Event{
		FacadeApproval(FacadeApprovalInfo{Tool: long, ToolClass: "write", ToolArgs: long}),
		FacadeRequest("POST", "m", "", "/mcp", 200, 0, 0).WithTool(long, "write", long),
	} {
		if len(ev.ToolArgs) > maxToolArgsBytes+len("…") || !utf8.ValidString(ev.ToolArgs) {
			t.Errorf("tool args %d bytes", len(ev.ToolArgs))
		}
		if len(ev.Tool) > maxToolBytes+len("…") || !utf8.ValidString(ev.Tool) {
			t.Errorf("tool %d bytes", len(ev.Tool))
		}
	}
}

func TestToolArgsRecordStaysBelowAtomicWriteLimit(t *testing.T) {
	long := strings.Repeat("x", 5000)
	ev := FacadeRequest("POST", "m", "ns", "/"+long, 200, 0, 1).WithTool(long, "write", long)
	line, err := marshalLine(ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(line) >= 4096 {
		t.Errorf("record is %d bytes", len(line))
	}
}
