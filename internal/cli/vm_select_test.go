package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/vm"
)

func gitInDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir,
		"-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// selectionFixture isolates state and config, creates a repository with a
// worktree under .worktrees/, and returns both paths.
func selectionFixture(t *testing.T) (repo, worktree string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitInDir(t, repo, "init", "-q")
	gitInDir(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	worktree = filepath.Join(repo, ".worktrees", "feat")
	gitInDir(t, repo, "worktree", "add", "-q", worktree, "-b", "feat")
	return repo, worktree
}

func writeVMProjectMap(t *testing.T, content string) string {
	t.Helper()
	path := vm.ProjectMapPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func createVMState(t *testing.T, name string) {
	t.Helper()
	root, err := vm.InstanceRoot(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.SaveState(vm.NewLayout(root).StatePath, &vm.State{SSHPort: 42022, Mode: vm.ModeOffline}); err != nil {
		t.Fatal(err)
	}
}

func TestVMFlagForms(t *testing.T) {
	cases := []struct {
		args     []string
		wantVM   bool
		wantName string
		wantOK   bool
	}{
		{[]string{}, false, "", true},
		{[]string{"--vm"}, true, "", true},
		{[]string{"--vm=true"}, true, "", true},
		{[]string{"--vm=false"}, false, "", true},
		{[]string{"--vm=yarp3"}, true, "yarp3", true},
		{[]string{"--vm=Bad_Name"}, false, "", false},
		{[]string{"--vm="}, false, "", false},
	}
	for _, c := range cases {
		for _, cmd := range []string{"start", "continue", "resume"} {
			opts, code := parseLaunchArgs(cmd, c.args, devnullEnv(t))
			if (code == ExitOK) != c.wantOK {
				t.Errorf("%s %v: code = %d, want ok=%v", cmd, c.args, code, c.wantOK)
				continue
			}
			if c.wantOK && (opts.vm != c.wantVM || opts.vmName != c.wantName) {
				t.Errorf("%s %v: vm=%v name=%q, want %v %q", cmd, c.args, opts.vm, opts.vmName, c.wantVM, c.wantName)
			}
		}
		p, ok := parseServeArgs(c.args, devnullEnv(t))
		if ok != c.wantOK {
			t.Errorf("serve %v: ok = %v, want %v", c.args, ok, c.wantOK)
			continue
		}
		if c.wantOK && (p.vm != c.wantVM || p.vmName != c.wantName) {
			t.Errorf("serve %v: vm=%v name=%q, want %v %q", c.args, p.vm, p.vmName, c.wantVM, c.wantName)
		}
	}
}

func TestVMFlagKeepsLaterFlags(t *testing.T) {
	opts, code := parseLaunchArgs("start", []string{"--vm", "--verbose"}, devnullEnv(t))
	if code != ExitOK || !opts.vm || !opts.verbose {
		t.Fatalf("code=%d vm=%v verbose=%v, want --vm not to swallow --verbose", code, opts.vm, opts.verbose)
	}
}

func TestSelectVM(t *testing.T) {
	repo, worktree := selectionFixture(t)
	t.Run("no entry is the default VM", func(t *testing.T) {
		got, err := selectVM("", worktree)
		if err != nil || got.name != vm.DefaultVMName {
			t.Fatalf("selectVM = (%+v, %v), want omac", got, err)
		}
	})
	writeVMProjectMap(t, repo+": yarp3\n")
	t.Run("worktree finds the entry of the main repo", func(t *testing.T) {
		got, err := selectVM("", worktree)
		if err != nil || got.name != "yarp3" {
			t.Fatalf("selectVM = (%+v, %v), want yarp3", got, err)
		}
	})
	t.Run("flag overrides the entry", func(t *testing.T) {
		got, err := selectVM("x", worktree)
		if err != nil || got.name != "x" {
			t.Fatalf("selectVM = (%+v, %v), want x", got, err)
		}
	})
	t.Run("directory outside the entry", func(t *testing.T) {
		got, err := selectVM("", t.TempDir())
		if err != nil || got.name != vm.DefaultVMName {
			t.Fatalf("selectVM = (%+v, %v), want omac", got, err)
		}
	})
	t.Run("broken map names the file", func(t *testing.T) {
		path := writeVMProjectMap(t, "{{{")
		_, err := selectVM("", worktree)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("selectVM = %v, want an error naming %s", err, path)
		}
		if _, err := selectVM("x", worktree); err != nil {
			t.Errorf("an explicit name must not read the map, got %v", err)
		}
	})
}

// recordingManager builds the manager seam for prepareVMAttach and records
// the VM names it was asked for.
func recordVMManagers(t *testing.T) *[]string {
	t.Helper()
	var names []string
	orig := newVMManager
	newVMManager = func(_ *Env, name string) (vmManager, error) {
		names = append(names, name)
		return &attachInfoManager{}, nil
	}
	t.Cleanup(func() { newVMManager = orig })
	return &names
}

type attachInfoManager struct{ fakeVMManager }

func (*attachInfoManager) PrepareAttach(context.Context) (vm.AttachInfo, error) {
	return vm.AttachInfo{SSHPort: 42022, PortBlock: 3}, nil
}

func TestPrepareVMAttachUsesMappedInstance(t *testing.T) {
	repo, worktree := selectionFixture(t)
	writeVMProjectMap(t, repo+": yarp3\n")
	createVMState(t, "yarp3")
	names := recordVMManagers(t)
	env := devnullEnv(t)
	env.Workdir = worktree

	info, release, err := prepareVMAttach(context.Background(), env, "", worktree)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "yarp3" || len(*names) != 1 || (*names)[0] != "yarp3" {
		t.Errorf("info.Name=%q managers=%v, want yarp3", info.Name, *names)
	}
	root, _ := vm.InstanceRoot("yarp3")
	if _, err := os.Stat(filepath.Join(vm.NewLayout(root).SessionsDir, strconv.Itoa(os.Getpid()))); err != nil {
		t.Errorf("session marker not in the mapped instance: %v", err)
	}
	release()
}

func TestPrepareVMAttachExplicitNameOverridesMapping(t *testing.T) {
	repo, worktree := selectionFixture(t)
	writeVMProjectMap(t, repo+": yarp3\n")
	createVMState(t, "yarp3")
	createVMState(t, "other")
	names := recordVMManagers(t)

	info, release, err := prepareVMAttach(context.Background(), devnullEnv(t), "other", worktree)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if info.Name != "other" || (*names)[0] != "other" {
		t.Errorf("info.Name=%q managers=%v, want other", info.Name, *names)
	}
}

func TestPrepareVMAttachUnknownInstanceAborts(t *testing.T) {
	repo, worktree := selectionFixture(t)
	for name, tc := range map[string]struct{ explicit, mapping string }{
		"flag":    {explicit: "ghost"},
		"mapping": {mapping: repo + ": ghost\n"},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.mapping != "" {
				writeVMProjectMap(t, tc.mapping)
			}
			names := recordVMManagers(t)
			_, release, err := prepareVMAttach(context.Background(), devnullEnv(t), tc.explicit, worktree)
			if err == nil || release != nil {
				t.Fatalf("prepareVMAttach = (release=%v, %v), want an error", release != nil, err)
			}
			if !strings.Contains(err.Error(), "omac vm up --name ghost") {
				t.Errorf("error %q lacks the hint to run `omac vm up --name ghost`", err)
			}
			if !errors.Is(err, vm.ErrNotCreated) || vmAttachExitCode(err) != ExitPrerequisiteMissing {
				t.Errorf("error %v must be a missing prerequisite", err)
			}
			if len(*names) != 0 {
				t.Errorf("a manager was built for %v before the abort", *names)
			}
			root, _ := vm.InstanceRoot("ghost")
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Errorf("state directory of the unknown VM was created (err %v)", err)
			}
		})
	}
}

func TestPrepareVMAttachMissingDefaultNamesNothingExtra(t *testing.T) {
	_, worktree := selectionFixture(t)
	orig := newVMManager
	newVMManager = func(*Env, string) (vmManager, error) { return &attachErrManager{err: vm.ErrNotCreated}, nil }
	t.Cleanup(func() { newVMManager = orig })
	_, _, err := prepareVMAttach(context.Background(), devnullEnv(t), "", worktree)
	if err == nil || strings.Contains(err.Error(), "--name") {
		t.Fatalf("err = %v, want the plain hint for the default VM", err)
	}
}

type attachErrManager struct {
	fakeVMManager
	err error
}

func (a *attachErrManager) PrepareAttach(context.Context) (vm.AttachInfo, error) {
	return vm.AttachInfo{}, a.err
}

func TestPrepareVMAttachHintNamesNonDefaultInstance(t *testing.T) {
	_, worktree := selectionFixture(t)
	createVMState(t, "yarp3")
	orig := newVMManager
	newVMManager = func(*Env, string) (vmManager, error) { return &attachErrManager{err: vm.ErrNotRunning}, nil }
	t.Cleanup(func() { newVMManager = orig })
	_, _, err := prepareVMAttach(context.Background(), devnullEnv(t), "yarp3", worktree)
	if !errors.Is(err, vm.ErrNotRunning) || !strings.Contains(err.Error(), "`omac vm up --name yarp3`") {
		t.Fatalf("err = %v, want ErrNotRunning with the hint naming yarp3", err)
	}
}

func TestVMExecUsesMappingOnHost(t *testing.T) {
	repo, worktree := selectionFixture(t)
	writeVMProjectMap(t, repo+": yarp3\n")
	calls := useFakeVMExec(t, nil, 0, nil)
	var resolved []string
	resolveVMConn = func(_ *Env, name string) (vm.ConnInfo, error) {
		resolved = append(resolved, name)
		return vm.ConnInfo{Target: vm.SSHTarget{User: "agent", Port: 42022}}, nil
	}
	env, _, stderr := vmTestEnv(t)
	env.Workdir = worktree
	if code := runVM([]string{"exec", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
	}
	if len(resolved) != 1 || resolved[0] != "yarp3" || len(*calls) != 1 {
		t.Errorf("resolved = %v, want [yarp3]", resolved)
	}
	resolved = nil
	if code := runVM([]string{"exec", "--name", "other", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
	}
	if len(resolved) != 1 || resolved[0] != "other" {
		t.Errorf("with --name: resolved = %v, want [other]", resolved)
	}
}

func TestVMExecWithoutEntryUsesDefault(t *testing.T) {
	_, worktree := selectionFixture(t)
	useFakeVMExec(t, nil, 0, nil)
	var resolved []string
	resolveVMConn = func(_ *Env, name string) (vm.ConnInfo, error) {
		resolved = append(resolved, name)
		return vm.ConnInfo{Target: vm.SSHTarget{User: "agent", Port: 42022}}, nil
	}
	env, _, _ := vmTestEnv(t)
	env.Workdir = worktree
	if code := runVM([]string{"exec", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if len(resolved) != 1 || resolved[0] != vm.DefaultVMName {
		t.Errorf("resolved = %v, want [omac]", resolved)
	}
}

func TestVMExecBrokenMapNamesFile(t *testing.T) {
	_, worktree := selectionFixture(t)
	path := writeVMProjectMap(t, "{{{")
	useFakeVMExec(t, nil, 0, nil)
	env, _, stderr := vmTestEnv(t)
	env.Workdir = worktree
	if code := runVM([]string{"exec", "--", "true"}, env); code == ExitOK {
		t.Fatal("exec ran despite a broken map")
	}
	if !strings.Contains(stderr(), path) {
		t.Errorf("stderr lacks %s:\n%s", path, stderr())
	}
}

// Inside a --vm session the exported OMAC_VM_* variables name the VM; neither
// --name nor the project map may redirect exec to another one.
func TestVMExecInSessionIgnoresNameAndMapping(t *testing.T) {
	repo, worktree := selectionFixture(t)
	writeVMProjectMap(t, repo+": mapped\n")
	keyDir := t.TempDir()
	t.Setenv(vm.EnvVMSSHPort, "43022")
	t.Setenv(vm.EnvVMSSHKey, filepath.Join(keyDir, "agent_key"))
	t.Setenv(vm.EnvVMUser, "agent")
	t.Setenv(vm.EnvVMName, "session-vm")

	orig, origMgr := runVMExecFn, newVMManager
	t.Cleanup(func() { runVMExecFn, newVMManager = orig, origMgr })
	newVMManager = func(*Env, string) (vmManager, error) {
		t.Error("exec must not build a VM manager")
		return nil, errors.New("no manager")
	}
	for _, args := range [][]string{
		{"exec", "--", "true"},
		{"exec", "--name", "session-vm", "--", "true"},
	} {
		var got []vm.ExecOpts
		runVMExecFn = func(_ context.Context, o vm.ExecOpts) (int, error) {
			got = append(got, o)
			return 0, nil
		}
		env, _, stderr := vmTestEnv(t)
		env.Workdir = worktree
		if code := runVM(args, env); code != ExitOK {
			t.Fatalf("%v: exit = %d; stderr:\n%s", args, code, stderr())
		}
		if len(got) != 1 || !got[0].Conn.FromSandbox || got[0].Conn.Target.Port != 43022 {
			t.Errorf("%v: conn = %+v, want the session's (port 43022)", args, got)
		}
	}
}

func TestVMListShowsCorruptInstanceAsError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	createVMState(t, "omac")
	root, _ := vm.InstanceRoot("bad")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	broken := vm.NewLayout(root).StatePath
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, stdout, stderr := vmTestEnv(t)
	if code := runVM([]string{"list"}, env); code != ExitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
	}
	out := stdout()
	if !strings.Contains(out, "omac") || !strings.Contains(out, "bad") || !strings.Contains(out, "error") {
		t.Errorf("list output lacks the rows:\n%s", out)
	}
	if !strings.Contains(stderr(), broken) {
		t.Errorf("stderr does not name %s:\n%s", broken, stderr())
	}
}

func TestVMManagerHintsNameNonDefaultInstance(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	orig := newVMManager
	newVMManager = func(*Env, string) (vmManager, error) { return &failingManager{err: vm.ErrNotCreated}, nil }
	t.Cleanup(func() { newVMManager = orig })
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"status", "--name", "yarp3"}, env); code == ExitOK {
		t.Fatal("status succeeded")
	}
	if !strings.Contains(stderr(), "`omac vm up --name yarp3`") {
		t.Errorf("stderr lacks the named hint:\n%s", stderr())
	}
}

type failingManager struct {
	fakeVMManager
	err error
}

func (f *failingManager) Status(context.Context) (vm.StatusReport, error) {
	return vm.StatusReport{}, f.err
}

func TestVMExecInSessionRejectsOtherName(t *testing.T) {
	t.Setenv(vm.EnvVMSSHPort, "43022")
	t.Setenv(vm.EnvVMSSHKey, filepath.Join(t.TempDir(), "agent_key"))
	t.Setenv(vm.EnvVMUser, "agent")
	t.Setenv(vm.EnvVMName, "session-vm")
	calls := useFakeVMExec(t, nil, 0, nil)
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"exec", "--name", "x", "--", "true"}, env); code != ExitMisuse {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, ExitMisuse, stderr())
	}
	for _, want := range []string{"session-vm", "--vm=x"} {
		if !strings.Contains(stderr(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr())
		}
	}
	if len(*calls) != 0 {
		t.Errorf("exec ran despite the misuse")
	}
}

// The sandbox blocks ~/.config/omac, so the project map cannot be read in a
// session without --vm; exec must then give ConnInfo's hint, not fail on it.
func TestVMExecUnreadableMapFallsThrough(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any file")
	}
	_, worktree := selectionFixture(t)
	path := writeVMProjectMap(t, "x: y\n")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })
	useFakeVMExec(t, nil, 0, nil)
	var resolved []string
	resolveVMConn = func(_ *Env, name string) (vm.ConnInfo, error) {
		resolved = append(resolved, name)
		return vm.ConnInfo{Target: vm.SSHTarget{User: "agent", Port: 42022}}, nil
	}
	env, _, stderr := vmTestEnv(t)
	env.Workdir = worktree
	if code := runVM([]string{"exec", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
	}
	if len(resolved) != 1 || resolved[0] != vm.DefaultVMName {
		t.Errorf("resolved = %v, want [omac]", resolved)
	}
}

func TestVMExecErrorNamesInstance(t *testing.T) {
	useFakeVMExec(t, nil, 0, errors.New("omac vm exec: connection lost (is the VM running? `omac vm status`)"))
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"exec", "--name", "yarp3", "--", "true"}, env); code != ExitIOError {
		t.Fatalf("exit = %d, want %d", code, ExitIOError)
	}
	if !strings.Contains(stderr(), "`omac vm status --name yarp3`") {
		t.Errorf("stderr does not name the instance:\n%s", stderr())
	}
}
