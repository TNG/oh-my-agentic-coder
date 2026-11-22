package mcppolicy

import (
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
)

func testPolicy(access Access) *Policy {
	return &Policy{
		Server:    "demo",
		Access:    access,
		Tools:     map[string]Class{"get": Read, "put": Write, "rm": Delete, "hidden": Off},
		Approvals: DefaultApprovals(),
	}
}

func TestAccessAllows(t *testing.T) {
	cases := []struct {
		access Access
		allows map[Class]bool
	}{
		{ReadOnly, map[Class]bool{Read: true}},
		{ReadWrite, map[Class]bool{Read: true, Write: true}},
		{ReadWriteDelete, map[Class]bool{Read: true, Write: true, Delete: true}},
		{"", map[Class]bool{}},
		{"bogus", map[Class]bool{}},
	}
	for _, tc := range cases {
		for _, c := range []Class{Read, Write, Delete, Off, "bogus"} {
			if got := tc.access.Allows(c); got != tc.allows[c] {
				t.Errorf("Access(%q).Allows(%q) = %v, want %v", tc.access, c, got, tc.allows[c])
			}
		}
	}
}

func TestValid(t *testing.T) {
	for _, c := range []Class{Read, Write, Delete, Off} {
		if !c.Valid() {
			t.Errorf("class %q should be valid", c)
		}
	}
	if Class("x").Valid() || Class("").Valid() {
		t.Error("unknown classes must be invalid")
	}
	if Access("").Valid() || Access("x").Valid() || !ReadOnly.Valid() {
		t.Error("access validity wrong")
	}
}

func TestApprovalDefaults(t *testing.T) {
	var zero Approvals
	want := map[Class]approval.Mode{Read: approval.None, Write: approval.Required, Delete: approval.Required, Off: approval.Required, "bogus": approval.Required}
	for c, m := range want {
		if got := zero.For(c); got != m {
			t.Errorf("default For(%q) = %q, want %q", c, got, m)
		}
	}
	custom := Approvals{Read: approval.Required, Write: approval.None, Delete: approval.None}
	if custom.For(Read) != approval.Required || custom.For(Write) != approval.None || custom.For(Delete) != approval.None {
		t.Error("explicit modes must win over defaults")
	}
}

func TestDecideMethods(t *testing.T) {
	p := testPolicy(ReadWriteDelete)
	allowed := []string{"initialize", "ping", "tools/list", "notifications/initialized", "notifications/tools/list_changed"}
	for _, m := range allowed {
		if d := p.Decide(Call{Method: m}); !d.Allow || d.NeedsApproval {
			t.Errorf("%s: %+v, want plain allow", m, d)
		}
	}
	refused := []string{"resources/read", "resources/list", "resources/subscribe", "prompts/get", "prompts/list",
		"completion/complete", "logging/setLevel", "sampling/createMessage", "notifications", "Tools/list", "tools/list/x", ""}
	for _, m := range refused {
		d := p.Decide(Call{Method: m})
		if d.Allow || d.Reason != ReasonUnlisted || d.Detail == "" {
			t.Errorf("%q: %+v, want refusal with %s", m, d, ReasonUnlisted)
		}
	}
}

func TestDecideResponseAllowed(t *testing.T) {
	if d := testPolicy(ReadOnly).Decide(Call{Response: true}); !d.Allow {
		t.Errorf("a response to a server request is allowed, got %+v", d)
	}
}

func TestDecideToolCall(t *testing.T) {
	cases := []struct {
		name     string
		access   Access
		tool     string
		allow    bool
		approval bool
		class    Class
		reason   string
	}{
		{"read needs no approval", ReadWriteDelete, "get", true, false, Read, ""},
		{"write needs approval", ReadWriteDelete, "put", true, true, Write, ""},
		{"delete needs approval", ReadWriteDelete, "rm", true, true, Delete, ""},
		{"unknown tool is refused", ReadWriteDelete, "nope", false, false, "", ReasonUnlisted},
		{"off tool is refused", ReadWriteDelete, "hidden", false, false, Off, ReasonUnlisted},
		{"empty tool is refused", ReadWriteDelete, "", false, false, "", ReasonUnlisted},
		{"read-only refuses write", ReadOnly, "put", false, false, Write, ReasonAccess},
		{"read-only refuses delete", ReadOnly, "rm", false, false, Delete, ReasonAccess},
		{"read-only allows read", ReadOnly, "get", true, false, Read, ""},
		{"read-write refuses delete", ReadWrite, "rm", false, false, Delete, ReasonAccess},
		{"read-write allows write", ReadWrite, "put", true, true, Write, ""},
		{"empty access refuses everything", "", "get", false, false, Read, ReasonAccess},
	}
	for _, tc := range cases {
		d := testPolicy(tc.access).Decide(Call{Method: "tools/call", Tool: tc.tool})
		if d.Allow != tc.allow || d.NeedsApproval != tc.approval || d.Class != tc.class || d.Reason != tc.reason {
			t.Errorf("%s: got %+v", tc.name, d)
		}
		if !d.Allow && d.Detail == "" {
			t.Errorf("%s: refusal has no detail", tc.name)
		}
	}
}

func TestDecideApprovalOverrides(t *testing.T) {
	p := testPolicy(ReadWriteDelete)
	p.Approvals = Approvals{Read: approval.Required, Write: approval.None, Delete: approval.None}
	if d := p.Decide(Call{Method: "tools/call", Tool: "get"}); !d.NeedsApproval {
		t.Error("read should need approval when configured")
	}
	if d := p.Decide(Call{Method: "tools/call", Tool: "put"}); d.NeedsApproval {
		t.Error("write should not need approval when configured none")
	}
}

func TestDecideIgnoresPin(t *testing.T) {
	p := testPolicy(ReadWriteDelete)
	p.Pin = nil
	if d := p.Decide(Call{Method: "tools/call", Tool: "get"}); !d.Allow {
		t.Errorf("a missing pin is the gateway's concern, got %+v", d)
	}
}

func TestVisible(t *testing.T) {
	p := testPolicy(ReadWrite)
	want := map[string]bool{"get": true, "put": true, "rm": false, "hidden": false, "nope": false}
	for tool, v := range want {
		if got := p.Visible(tool); got != v {
			t.Errorf("Visible(%q) = %v, want %v", tool, got, v)
		}
	}
}
