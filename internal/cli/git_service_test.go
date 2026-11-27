package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
	"github.com/TNG/oh-my-agentic-coder/internal/approver"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestGitClientOnlyAcceptsSupportedForms(t *testing.T) {
	for _, args := range [][]string{{"stage", "--", "file"}, {"stage", "--", "-literal"}, {"commit", "-m", "hello", "--signoff"}, {"branch", "topic"}, {"switch", "topic"}, {"status"}} {
		if _, _, err := parseGitRequest(args, io.Discard); err != nil {
			t.Errorf("rejected %v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"push"}, {"config", "core.hooksPath", "bad"}, {"rebase", "--exec", "bad"}, {"stage", "-A"}, {"commit", "--amend", "-m", "bad"}, {"branch", "-D", "main"}, {"switch", "--force", "main"}, {"checkout", "main"}, {"-c", "core.hooksPath=bad", "commit"}} {
		if _, _, err := parseGitRequest(args, io.Discard); err == nil {
			t.Errorf("accepted unsupported %v", args)
		}
	}
}

func TestGitServicePermissionsAreSeparateAndDefaultClosed(t *testing.T) {
	r := gitServiceRoute(nil)
	if config.ValidSkillName(r.Skill) == nil {
		t.Fatal("a project skill could inherit Git service approvals")
	}
	for _, action := range []string{"stage", "commit", "branches", "switch"} {
		e, ok := r.Approvals.Lookup("POST", []string{action})
		if !ok || e.Approval != approval.Required || e.Match != "POST /"+action {
			t.Errorf("bad permission for %s: %+v", action, e)
		}
	}
	for _, p := range [][]string{{"status"}, {"operations", "id"}} {
		if e, ok := r.Approvals.Lookup("GET", p); !ok || e.Approval != approval.None {
			t.Errorf("read permission: %+v", e)
		}
	}
	if e, _ := r.Approvals.Lookup("POST", []string{"run"}); e.Approval != approval.Required {
		t.Fatal("unlisted endpoint admitted")
	}
}

func TestGitServiceLaunchFailsClosed(t *testing.T) {
	plan := sandboxPlan{Native: true, Policy: sandboxprofile.DefaultProfile()}
	base := launchOpts{label: "start", gitService: "prototype", gitName: "Test", gitEmail: "test@example.invalid"}
	if err := validateGitServiceLaunch(base, plan); runtime.GOOS == "linux" && err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*launchOpts){func(o *launchOpts) { o.noSandbox = true }, func(o *launchOpts) { o.keepRunning = true }, func(o *launchOpts) { o.gitService = "other" }, func(o *launchOpts) { o.gitEmail = "" }, func(o *launchOpts) { o.label = "continue" }} {
		o := base
		change(&o)
		if err := validateGitServiceLaunch(o, plan); err == nil {
			t.Fatalf("invalid launch admitted: %+v", o)
		}
	}
	if err := validateGitServiceLaunch(base, sandboxPlan{}); err == nil {
		t.Fatal("external launcher admitted")
	}
	env := devnullEnv(t)
	opts, code := parseLaunchArgs("start", []string{"--git-service=prototype", "--git-user-name=Test", "--git-user-email=test@example.invalid", "--git-wrapper"}, env)
	if code != ExitOK || !opts.gitWrapper || opts.gitService != "prototype" {
		t.Fatalf("flags: %+v, %d", opts, code)
	}
	if err := validateGitServiceLaunch(launchOpts{harness: config.DefaultHarness()}, sandboxPlan{}); err != nil {
		t.Fatal("ordinary launch changed")
	}
}

func TestGitStagePathsRespectClientDirectory(t *testing.T) {
	paths, err := gitStagePaths("/repo", "/repo/sub", []string{"file", "../rootfile"})
	if err != nil || len(paths) != 2 || paths[0] != "sub/file" || paths[1] != "rootfile" {
		t.Fatalf("relative paths: %v %v", paths, err)
	}
	for _, cwd := range []string{"/repo-other", "/"} {
		if _, err := gitStagePaths("/repo", cwd, []string{"file"}); err == nil {
			t.Fatal("outside checkout accepted")
		}
	}
}

func TestGitClientRejectsNestedRepositoryDirectory(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(filepath.Join(nested, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := gitClientDirectory(root, nested); err == nil {
		t.Fatal("nested repository would mutate parent checkout")
	}
}

type gitPermissionChooser struct{ calls int }

func (c *gitPermissionChooser) Notify(string, string) {}
func (c *gitPermissionChooser) Choose(context.Context, string, string, []string, string) (string, error) {
	c.calls++
	if c.calls == 1 {
		return approver.LabelSession, nil
	}
	return approver.LabelDeny, nil
}

func TestGitServiceApprovalsStayWithinOperationProjectAndPrincipal(t *testing.T) {
	chooser := &gitPermissionChooser{}
	scopes := map[string]approver.Scope{"one": projectScope(t.TempDir()), "two": projectScope(t.TempDir())}
	a := approver.New(approver.Config{Chooser: chooser, ScopeFor: func(ns string) approver.Scope { return scopes[ns] }})
	route := gitServiceRoute(nil)
	request := func(action, project, skill string) facade.ApprovalDecision {
		e, _ := route.Approvals.Lookup("POST", []string{action})
		return a.Approve(t.Context(), facade.ApprovalRequest{Skill: skill, Mount: route.Mount, Namespace: project, Method: "POST", Path: "/" + action, Match: e.Match})
	}
	if !request("stage", "one", route.Skill).Allow || !request("stage", "one", route.Skill).Allow || chooser.calls != 1 {
		t.Fatal("stage session permission was not reused")
	}
	for _, args := range [][3]string{{"commit", "one", route.Skill}, {"switch", "one", route.Skill}, {"branches", "one", route.Skill}, {"stage", "two", route.Skill}, {"stage", "one", "builtin-git"}} {
		if request(args[0], args[1], args[2]).Allow {
			t.Fatalf("approval crossed boundary: %v", args)
		}
	}
}
