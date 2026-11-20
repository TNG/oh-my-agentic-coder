package cli

import (
	"os"
	"path/filepath"
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
	if strings.Contains(out.String(), "not declared") {
		t.Errorf("declared entry listed as not declared: %q", out.String())
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

func TestApprovalsListMarksUnlistedRoutes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	id := approver.VerdictID("wd-1", "tng-slack", "GET /messages/123", "unlisted")
	if err := approver.NewStore("").Add(approver.Verdict{ID: id, Scope: "wd-1", Skill: "tng-slack", Match: "GET /messages/123", Approval: "unlisted", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	outF, _ := os.CreateTemp(t.TempDir(), "out")
	errF, _ := os.CreateTemp(t.TempDir(), "err")
	out := &capture{outF}
	if code := runApprovals([]string{"list"}, &Env{Stdout: outF, Stderr: errF}); code != ExitOK || !strings.Contains(out.String(), "GET /messages/123 (not declared)") {
		t.Errorf("list: code=%d out=%q", code, out.String())
	}
}

func TestApprovalsSummary(t *testing.T) {
	if s := approvalsSummary(&config.Meta{Sidecar: &config.SidecarMeta{}}); !strings.Contains(s, "no approvals block") {
		t.Errorf("ungated skill: %q", s)
	}
	m := &config.Meta{Sidecar: &config.SidecarMeta{Approvals: &config.ApprovalsSpec{Endpoints: []config.ApprovalSpec{
		{Match: "GET /a", Approval: approval.None}, {Match: "POST /b", Approval: approval.Required},
	}}}}
	if s := approvalsSummary(m); !strings.Contains(s, "1 endpoint(s) run without asking") {
		t.Errorf("gated skill: %q", s)
	}
	empty := &config.Meta{Sidecar: &config.SidecarMeta{Approvals: &config.ApprovalsSpec{}}}
	if s := approvalsSummary(empty); !strings.Contains(s, "every request to this skill asks you first") {
		t.Errorf("empty block: %q", s)
	}
}

// capture reads back what a command wrote to a temp file and resets it.
type capture struct{ f *os.File }

func (c *capture) String() string {
	b, _ := os.ReadFile(c.f.Name())
	return string(b)
}
func (c *capture) Reset() { c.f.Truncate(0); c.f.Seek(0, 0) }

func approvalsEnv(t *testing.T) (*Env, *capture, *capture) {
	t.Helper()
	outF, _ := os.CreateTemp(t.TempDir(), "out")
	errF, _ := os.CreateTemp(t.TempDir(), "err")
	return &Env{Stdout: outF, Stderr: errF}, &capture{outF}, &capture{errF}
}

func TestApprovalsArguments(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for name, c := range map[string]struct {
		args      []string
		code      int
		usageOn   string // "out", "err" or ""
		errSubstr string
	}{
		"bare":            {nil, ExitMisuse, "err", ""},
		"help":            {[]string{"--help"}, ExitOK, "out", ""},
		"list --help":     {[]string{"list", "--help"}, ExitOK, "out", ""},
		"revoke -h":       {[]string{"revoke", "-h"}, ExitOK, "out", ""},
		"list extra":      {[]string{"list", "--all"}, ExitMisuse, "", "unexpected argument"},
		"revoke no id":    {[]string{"revoke"}, ExitMisuse, "", "exactly one id"},
		"revoke two ids":  {[]string{"revoke", "a", "b"}, ExitMisuse, "", "exactly one id"},
		"revoke flag":     {[]string{"revoke", "--force"}, ExitMisuse, "", "exactly one id"},
		"unknown verb":    {[]string{"purge"}, ExitMisuse, "err", "unknown verb"},
		"unknown id":      {[]string{"revoke", "nope"}, ExitMisuse, "", "no stored approval"},
		"empty list":      {[]string{"list"}, ExitOK, "", ""},
		"list after help": {[]string{"list", "--", "x"}, ExitMisuse, "", "unexpected argument"},
	} {
		env, out, errb := approvalsEnv(t)
		code := runApprovals(c.args, env)
		if code != c.code {
			t.Errorf("%s: code %d, want %d (err %q)", name, code, c.code, errb.String())
		}
		usage := map[string]*capture{"out": out, "err": errb}[c.usageOn]
		if usage != nil && !strings.Contains(usage.String(), "Usage: omac approvals") {
			t.Errorf("%s: usage not on std%s: out=%q err=%q", name, c.usageOn, out.String(), errb.String())
		}
		if c.errSubstr != "" && !strings.Contains(errb.String(), c.errSubstr) {
			t.Errorf("%s: stderr %q lacks %q", name, errb.String(), c.errSubstr)
		}
	}
}

// Without a config directory there is no store; list must not report an
// empty one as if it had looked.
func TestApprovalsWithoutConfigDirIsAnError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	for _, verb := range [][]string{{"list"}, {"revoke", "x"}} {
		env, out, errb := approvalsEnv(t)
		if code := runApprovals(verb, env); code != ExitIOError || strings.Contains(out.String(), "no stored approvals") {
			t.Errorf("%v: code=%d out=%q err=%q", verb, code, out.String(), errb.String())
		}
	}
}

func TestApprovalsListCorruptStoreIsAnError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	os.MkdirAll(filepath.Join(dir, "omac"), 0o700)
	os.WriteFile(filepath.Join(dir, "omac", "endpoint-verdicts.json"), []byte("{"), 0o600)
	env, _, errb := approvalsEnv(t)
	if code := runApprovals([]string{"list"}, env); code != ExitIOError || !strings.Contains(errb.String(), "parse endpoint verdicts") {
		t.Errorf("code=%d err=%q", code, errb.String())
	}
}

// The PROJECT column names the directory, not the hash the scope is keyed
// on, and says when an answer covers every project or only one route.
func TestApprovalsListNamesProjectAndKind(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	st := approver.NewStore("")
	for _, v := range []approver.Verdict{
		{ID: "a1", Scope: "wd-1", Dir: "/home/me/project", Skill: "s", Match: "POST /send", Approval: approver.KindRequired},
		{ID: "b2", Scope: "global", Skill: "s", Match: "GET /x", Approval: approver.KindRequired},
		{ID: "c3", Scope: "wd-1", Dir: "/home/me/project", Skill: "s", Match: "DELETE /admin/u", Approval: approver.KindCatchAll},
	} {
		if err := st.Add(v); err != nil {
			t.Fatal(err)
		}
	}
	env, out, _ := approvalsEnv(t)
	if code := runApprovals([]string{"list"}, env); code != ExitOK {
		t.Fatal(code)
	}
	for _, want := range []string{"PROJECT", "/home/me/project", "all projects (user-global skill in omac serve)", "DELETE /admin/u (this route only)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("list lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "wd-1") {
		t.Errorf("list shows the scope hash:\n%s", out.String())
	}
}
