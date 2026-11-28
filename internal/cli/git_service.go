package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/gitservice"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

const gitServiceBriefing = `
Git service prototype: this checkout's entire .git directory is read-only.
Use omac git status, omac git stage -- <files>, omac git unstage -- <files>,
omac git commit -m <message> [--signoff], omac git branch <name>, and
omac git switch <existing-branch>. Unstage preserves working-tree edits.
Ordinary git status/log/diff remain local. Hooks, signing, filters, network
operations and advanced Git commands are unsupported. Stop file-generating
watchers and concurrent edits before switching branches. A lost response is
not permission to retry with a new operation ID: inspect omac git status.
This protects only the enrolled checkout, not newly initialized repositories.
Existing host hooks may execute agent-edited source scripts; inspect them before
running hook-triggering Git commands outside the sandbox.
`

func validateGitServiceLaunch(opts launchOpts, plan sandboxPlan) error {
	if opts.gitService == "" {
		if opts.gitName != "" || opts.gitEmail != "" || opts.gitWrapper {
			return fmt.Errorf("Git service options require --git-service=prototype")
		}
		return nil
	}
	if opts.gitService != "prototype" {
		return fmt.Errorf("--git-service supports only prototype")
	}
	if runtime.GOOS != "linux" || opts.noSandbox || !plan.Native || plan.Policy == nil || opts.keepRunning || opts.label != "start" {
		return fmt.Errorf("Git prototype requires Linux, omac start, the built-in sandbox and no --no-sandbox or --keep-running")
	}
	if opts.gitName == "" || opts.gitEmail == "" {
		return fmt.Errorf("Git prototype requires --git-user-name and --git-user-email")
	}
	return nil
}

func gitServiceRoute(s *gitservice.Service) facade.Route {
	entries := []approval.Entry{
		{Match: "GET /status", Approval: approval.None},
		{Match: "GET /operations/{id}", Approval: approval.None},
		{Match: "POST /stage", Approval: approval.Required, Description: "Stages explicit files in this repository; no hooks or filters."},
		{Match: "POST /unstage", Approval: approval.Required, Description: "Unstages explicit files without changing working-tree contents."},
		{Match: "POST /commit", Approval: approval.Required, Description: "Creates a local commit; this approves the operation, not a particular diff."},
		{Match: "POST /branches", Approval: approval.Required, Description: "Creates a local branch without replacing another branch."},
		{Match: "POST /switch", Approval: approval.Required, Description: "Switches local branches and updates source files; requires a clean working tree."},
	}
	set, err := approval.Compile(entries)
	if err != nil {
		set = approval.AskAll()
	}
	return facade.Route{Mount: "git", Skill: "builtin:git", Owner: "builtin:git", Handler: s, Approvals: set, MaxBodyBytes: 1 << 20, HealthPath: "/status"}
}

func configureGitService(s *gitservice.Service, plan sandboxPlan, argv []string) error {
	if len(argv) < 4 || argv[1] != "sandbox" || argv[2] != "run" {
		return fmt.Errorf("Git prototype requires the native sandbox launcher")
	}
	flags, err := sandboxprofile.ParseFlags(argv[3:])
	if err != nil {
		return err
	}
	if flags.Learn {
		return fmt.Errorf("Git prototype cannot use learn mode")
	}
	if !flags.GitReadOnly {
		return fmt.Errorf("Git prototype requires read-only Git metadata")
	}
	profile, _ := sandboxprofile.Merge(plan.Policy, flags)
	if len(profile.DenyScanAcceptIncomplete()) != 0 {
		return fmt.Errorf("Git prototype requires a complete secret-file scan; continue scanning or raise filesystem.deny_scan.max_entries")
	}
	grants, err := sandboxrun.ResolveGrants(profile, s.Workdir(), io.Discard)
	if err != nil {
		return err
	}
	if err := grants.ValidateReadOnlyGit(); err != nil {
		return err
	}
	return s.ConfigureDenies(grants.ProtectedPaths)
}

func gitServiceAudit(a audit.Auditor, dir string) func(string, string, string) {
	return func(action, result, head string) {
		a.Emit(audit.Event{Type: audit.TypeGitOperation, Action: action, Dir: dir, Result: result, Detail: head})
	}
}

func createGitWrapper() (string, func(), error) {
	dir, err := os.MkdirTemp("/tmp", "omac-git-client-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	self, err := os.Executable()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	// The client has no host privileges; only the facade can authorize mutations.
	quoted := "'" + strings.ReplaceAll(self, "'", "'\\''") + "'"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexec "+quoted+" git --compat \"$@\"\n"), 0500); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}
