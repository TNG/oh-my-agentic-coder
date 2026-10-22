package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/vm"
)

// useFakeVMExec swaps both exec seams and the manager seam; the manager
// fake fails the test if exec ever builds a manager. The returned pointer
// holds the options of the last runVMExecFn call.
func useFakeVMExec(t *testing.T, connErr error, code int, execErr error) *[]vm.ExecOpts {
	t.Helper()
	origConn, origRun, origMgr := resolveVMConn, runVMExecFn, newVMManager
	t.Cleanup(func() { resolveVMConn, runVMExecFn, newVMManager = origConn, origRun, origMgr })
	conn := vm.ConnInfo{Target: vm.SSHTarget{User: "agent", Port: 42022}}
	resolveVMConn = func(*Env) (vm.ConnInfo, error) { return conn, connErr }
	var calls []vm.ExecOpts
	runVMExecFn = func(_ context.Context, o vm.ExecOpts) (int, error) {
		calls = append(calls, o)
		return code, execErr
	}
	newVMManager = func(*Env) (vmManager, error) {
		t.Error("exec must not build a VM manager")
		return nil, errors.New("no manager")
	}
	return &calls
}

func TestVMExecPassesWorkdirAndCommand(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"exec", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, ExitOK, stderr())
	}
	if len(*calls) != 1 {
		t.Fatalf("runVMExecFn calls = %d, want 1", len(*calls))
	}
	o := (*calls)[0]
	if o.Workdir != env.Workdir {
		t.Errorf("Workdir = %q, want %q", o.Workdir, env.Workdir)
	}
	if !reflect.DeepEqual(o.Cmd, []string{"true"}) {
		t.Errorf("Cmd = %q, want [true]", o.Cmd)
	}
	if o.Conn.Target.Port != 42022 || o.Run == nil || o.SSH == nil {
		t.Errorf("Conn/Run/SSH not wired: %+v", o)
	}
	if o.Stdin != env.Stdin || o.Stdout != env.Stdout || o.Stderr != env.Stderr {
		t.Errorf("stdio not passed through")
	}
}

func TestVMExecCwdAndEnv(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	env, _, stderr := vmTestEnv(t)
	args := []string{"exec", "--cwd", "backend", "--env", "A=1", "--env=B=2", "--", "./gradlew", "test"}
	if code := runVM(args, env); code != ExitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
	}
	o := (*calls)[0]
	if o.Cwd != "backend" {
		t.Errorf("Cwd = %q, want backend", o.Cwd)
	}
	if !reflect.DeepEqual(o.Env, []string{"A=1", "B=2"}) {
		t.Errorf("Env = %q", o.Env)
	}
	if !reflect.DeepEqual(o.Cmd, []string{"./gradlew", "test"}) {
		t.Errorf("Cmd = %q", o.Cmd)
	}
}

func TestVMExecEnvFromHost(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	t.Setenv("OMAC_TEST_EXEC_VAR", "hello")
	env, _, _ := vmTestEnv(t)
	if code := runVM([]string{"exec", "--cwd=sub", "--env", "OMAC_TEST_EXEC_VAR", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	o := (*calls)[0]
	if o.Cwd != "sub" || !reflect.DeepEqual(o.Env, []string{"OMAC_TEST_EXEC_VAR=hello"}) {
		t.Errorf("Cwd = %q, Env = %q", o.Cwd, o.Env)
	}
}

func TestVMExecWithoutSeparator(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	env, _, _ := vmTestEnv(t)
	if code := runVM([]string{"exec", "--cwd", "x", "ls", "-la"}, env); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if got := (*calls)[0].Cmd; !reflect.DeepEqual(got, []string{"ls", "-la"}) {
		t.Errorf("Cmd = %q, want [ls -la]", got)
	}
}

func TestVMExecMisuse(t *testing.T) {
	for name, args := range map[string][]string{
		"no command":       {"exec"},
		"empty after --":   {"exec", "--cwd", "x", "--"},
		"cwd no value":     {"exec", "--cwd"},
		"unknown flag":     {"exec", "--bogus", "--", "true"},
		"env unset":        {"exec", "--env", "OMAC_TEST_SURELY_UNSET_VAR", "--", "true"},
		"env invalid name": {"exec", "--env", "1BAD=x", "--", "true"},
	} {
		t.Run(name, func(t *testing.T) {
			calls := useFakeVMExec(t, nil, 0, nil)
			env, _, stderr := vmTestEnv(t)
			if code := runVM(args, env); code != ExitMisuse {
				t.Fatalf("exit = %d, want %d; stderr:\n%s", code, ExitMisuse, stderr())
			}
			if len(*calls) != 0 {
				t.Fatalf("runVMExecFn called on misuse")
			}
		})
	}
}

func TestVMExecRemoteExitCode(t *testing.T) {
	useFakeVMExec(t, nil, 2, nil)
	env, _, _ := vmTestEnv(t)
	if code := runVM([]string{"exec", "--", "false"}, env); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestVMExecNotCreated(t *testing.T) {
	calls := useFakeVMExec(t, vm.ErrNotCreated, 0, nil)
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"exec", "--", "true"}, env); code != ExitPrerequisiteMissing {
		t.Fatalf("exit = %d, want %d", code, ExitPrerequisiteMissing)
	}
	if !strings.Contains(stderr(), "omac vm up") {
		t.Errorf("stderr lacks hint:\n%s", stderr())
	}
	if len(*calls) != 0 {
		t.Errorf("runVMExecFn called without a VM")
	}
}

func TestVMExecErrorIsIOError(t *testing.T) {
	useFakeVMExec(t, nil, 1, errors.New("omac vm exec: rsync to the VM failed: boom"))
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"exec", "--", "true"}, env); code != ExitIOError {
		t.Fatalf("exit = %d, want %d", code, ExitIOError)
	}
	if !strings.Contains(stderr(), "rsync to the VM failed") {
		t.Errorf("stderr lacks error:\n%s", stderr())
	}
}

func TestVMExecHelp(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	for _, h := range []string{"-h", "--help"} {
		env, stdout, _ := vmTestEnv(t)
		if code := runVM([]string{"exec", h}, env); code != ExitOK {
			t.Fatalf("%s: exit = %d", h, code)
		}
		out := stdout()
		for _, want := range []string{"omac vm exec", "--cwd", "--env", "Copies the worktree", "runs the command", "stay in the VM"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: stdout lacks %q:\n%s", h, want, out)
			}
		}
	}
	if len(*calls) != 0 {
		t.Errorf("help ran the command")
	}
}

func TestVMExecVerboseLogs(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	env, _, _ := vmTestEnv(t)
	if code := runVM([]string{"exec", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if code := runVM([]string{"exec", "--verbose", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if (*calls)[0].Log != nil || (*calls)[1].Log == nil {
		t.Errorf("Log should be nil without --verbose and set with it")
	}
}
