package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
	"github.com/TNG/oh-my-agentic-coder/internal/approver"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

func TestApprovalsListAndRevoke(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	st := approver.NewStore("")
	id := approver.VerdictID("wd-1", "tng-slack", "POST /tools/x", "required")
	if err := st.Add(approver.Verdict{ID: id, Scope: "wd-1", Skill: "tng-slack", Match: "POST /tools/x", Approval: "required", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	outF, _ := os.CreateTemp(t.TempDir(), "out")
	errF, _ := os.CreateTemp(t.TempDir(), "err")
	env := &Env{Stdout: outF, Stderr: errF}
	out := &capture{outF}
	errb := &capture{errF}
	if code := runApprovals([]string{"list"}, env); code != ExitOK || !strings.Contains(out.String(), "tng-slack") || !strings.Contains(out.String(), id) {
		t.Fatalf("list: code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := runApprovals([]string{"revoke", id}, env); code != ExitOK {
		t.Fatalf("revoke: code=%d err=%q", code, errb.String())
	}
	out.Reset()
	runApprovals([]string{"list"}, env)
	if !strings.Contains(out.String(), "no stored approvals") {
		t.Errorf("after revoke: %q", out.String())
	}
	if code := runApprovals([]string{"revoke", "nope"}, env); code != ExitMisuse {
		t.Errorf("revoke of unknown id: code=%d", code)
	}
}

func TestApprovalsSummary(t *testing.T) {
	if s := approvalsSummary(&config.Meta{Sidecar: &config.SidecarMeta{}}); !strings.Contains(s, "ungated") {
		t.Errorf("ungated skill: %q", s)
	}
	m := &config.Meta{Sidecar: &config.SidecarMeta{Approvals: &config.ApprovalsSpec{Version: 1, Endpoints: []config.ApprovalSpec{
		{Match: "GET /a", Approval: approval.None}, {Match: "POST /b", Approval: approval.Required},
	}}}}
	if s := approvalsSummary(m); !strings.Contains(s, "2 endpoint(s) declared, 1 ask you first") {
		t.Errorf("gated skill: %q", s)
	}
}

// capture reads back what a command wrote to a temp file and resets it.
type capture struct{ f *os.File }

func (c *capture) String() string {
	b, _ := os.ReadFile(c.f.Name())
	return string(b)
}
func (c *capture) Reset() { c.f.Truncate(0); c.f.Seek(0, 0) }
