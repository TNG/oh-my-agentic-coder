package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/vm"
)

// useFakeVMExec swaps the exec, state and manager seams; the manager
// fake fails the test if exec ever builds a manager. The returned pointer
// holds the options of every runVMExecFn call, in order.
func useFakeVMExec(t *testing.T, connErr error, code int, execErr error) *[]vm.ExecOpts {
	t.Helper()
	origConn, origRun, origMgr, origState := resolveVMConn, runVMExecFn, newVMManager, loadVMState
	t.Cleanup(func() {
		resolveVMConn, runVMExecFn, newVMManager, loadVMState = origConn, origRun, origMgr, origState
	})
	conn := vm.ConnInfo{Target: vm.SSHTarget{User: "agent", Port: 42022}}
	resolveVMConn = func(*Env, string) (vm.ConnInfo, error) { return conn, connErr }
	// --env needs an online VM; tests about other refusals override this.
	loadVMState = func(string) (*vm.State, error) { return &vm.State{Mode: vm.ModeOnline}, nil }
	var calls []vm.ExecOpts
	runVMExecFn = func(_ context.Context, o vm.ExecOpts) (int, error) {
		calls = append(calls, o)
		return code, execErr
	}
	newVMManager = func(*Env, string) (vmManager, error) {
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

const tokenNoticeStart = "omac vm exec: the build in the VM can read "

func TestVMExecEnvPrintsTokenNoticeBeforeRunning(t *testing.T) {
	const tail = "If a session you do not trust used the VM since your last `omac vm reset`, stop and run it first:\n"
	for name, tc := range map[string]struct {
		envArgs []string
		want    string
	}{
		"one name": {
			[]string{"--env", "MY_REPO_TOKEN"},
			tokenNoticeStart + "MY_REPO_TOKEN.\n" + tail +
				"files that session left in the VM run with this token.\n",
		},
		"two names": {
			[]string{"--env", "MY_REPO_TOKEN", "--env", "B=VALUE-MUST-NOT-PRINT-b"},
			tokenNoticeStart + "MY_REPO_TOKEN and B.\n" + tail +
				"files that session left in the VM run with these tokens.\n",
		},
		"three names, one repeated": {
			[]string{"--env", "MY_REPO_TOKEN", "--env", "B=VALUE-MUST-NOT-PRINT-b", "--env", "C=VALUE-MUST-NOT-PRINT-c", "--env", "B=VALUE-MUST-NOT-PRINT-b2"},
			tokenNoticeStart + "MY_REPO_TOKEN, B and C.\n" + tail +
				"files that session left in the VM run with these tokens.\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			calls := useFakeVMExec(t, nil, 0, nil)
			t.Setenv("MY_REPO_TOKEN", "VALUE-MUST-NOT-PRINT-host")
			env, _, stderr := vmTestEnv(t)
			var stderrAtRun string
			runVMExecFn = func(_ context.Context, o vm.ExecOpts) (int, error) {
				*calls = append(*calls, o)
				stderrAtRun = stderr()
				return 0, nil
			}
			args := append(append([]string{"exec"}, tc.envArgs...), "--", "true")
			if code := runVM(args, env); code != ExitOK {
				t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
			}
			if stderrAtRun != tc.want {
				t.Errorf("stderr before the command ran:\n%s\nwant:\n%s", stderrAtRun, tc.want)
			}
			if strings.Contains(stderr(), "VALUE-MUST-NOT-PRINT") {
				t.Errorf("stderr shows a value:\n%s", stderr())
			}
		})
	}
}

const tokenConfirmPrompt = vmExecEnvPrompt + "\n"

// startVMExecAtPrompt runs `omac vm exec --env` with a pipe as its
// terminal stdin, waits until the prompt is on stderr, and returns the
// pipe's write end and the channel that receives the exit code.
func startVMExecAtPrompt(t *testing.T, env *Env, stderr func() string) (*os.File, <-chan int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	env.Stdin = r
	useTerminals(t, r, env.Stderr)
	done := make(chan int, 1)
	go func() { done <- runVM([]string{"exec", "--env", "MY_REPO_TOKEN", "--", "true"}, env) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.HasSuffix(stderr(), tokenConfirmPrompt) {
		if time.Now().After(deadline) {
			t.Fatalf("no prompt; stderr:\n%s", stderr())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return w, done
}

// exitCodeWithin fails the test instead of hanging the package run when
// exec does not stop.
func exitCodeWithin(t *testing.T, done <-chan int, after string) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(10 * time.Second):
		t.Fatalf("exec did not stop after %s", after)
		return 0
	}
}

// useFakeTerminalStdin makes env.Stdin read input, and makes the terminal
// check accept it and env.Stderr, where the prompt goes.
func useFakeTerminalStdin(t *testing.T, env *Env, input string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	env.Stdin = f
	useTerminals(t, f, env.Stderr)
}

func TestVMExecEnvOnTerminalWaitsForEnter(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	t.Setenv("MY_REPO_TOKEN", "x")
	env, _, stderr := vmTestEnv(t)
	useFakeTerminalStdin(t, env, "\ninput for the command")
	var stderrAtRun, stdinAtRun string
	runVMExecFn = func(_ context.Context, o vm.ExecOpts) (int, error) {
		*calls = append(*calls, o)
		stderrAtRun = stderr()
		b, err := io.ReadAll(o.Stdin)
		if err != nil {
			t.Error(err)
		}
		stdinAtRun = string(b)
		return 0, nil
	}
	if code := runVM([]string{"exec", "--env", "MY_REPO_TOKEN", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
	}
	if len(*calls) != 1 {
		t.Fatalf("runVMExecFn calls = %d, want 1", len(*calls))
	}
	if !strings.HasPrefix(stderrAtRun, tokenNoticeStart) || !strings.HasSuffix(stderrAtRun, "with this token.\n"+tokenConfirmPrompt) {
		t.Errorf("stderr before the command ran:\n%s\nwant the notice, then %q", stderrAtRun, tokenConfirmPrompt)
	}
	if stdinAtRun != "input for the command" {
		t.Errorf("command's stdin = %q, want the input after the Enter", stdinAtRun)
	}
}

// The prompt can wait for hours; a session may attach to the VM meanwhile.
func TestVMExecEnvRefusedWhenVMWentOfflineDuringPrompt(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	t.Setenv("MY_REPO_TOKEN", "x")
	var mode atomic.Value
	mode.Store(vm.ModeOnline)
	loadVMState = func(string) (*vm.State, error) { return &vm.State{Mode: mode.Load().(string)}, nil }
	env, _, stderr := vmTestEnv(t)
	w, done := startVMExecAtPrompt(t, env, stderr)
	mode.Store(vm.ModeOffline)
	if _, err := io.WriteString(w, "\n"); err != nil {
		t.Fatal(err)
	}
	if code := exitCodeWithin(t, done, "Enter"); code != ExitMisuse {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, ExitMisuse, stderr())
	}
	if len(*calls) != 0 {
		t.Errorf("the command ran in a VM that is no longer online")
	}
	if !strings.Contains(stderr(), "run `omac vm online` first") {
		t.Errorf("stderr lacks the reason:\n%s", stderr())
	}
	if strings.Contains(stderr(), "Usage:") {
		t.Errorf("stderr shows the usage for a valid call:\n%s", stderr())
	}
}

// Only a bare Enter is consent: a typed "n" or a pasted next command line
// must not send the token.
func TestVMExecEnvOnTerminalStopsWithoutBareEnter(t *testing.T) {
	for name, input := range map[string]string{
		"end of input":       "",
		"typed n":            "n\n",
		"pasted next line":   "omac vm offline\n",
		"line without Enter": "y",
	} {
		t.Run(name, func(t *testing.T) {
			calls := useFakeVMExec(t, nil, 0, nil)
			t.Setenv("MY_REPO_TOKEN", "x")
			env, _, stderr := vmTestEnv(t)
			useFakeTerminalStdin(t, env, input)
			if code := runVM([]string{"exec", "--env", "MY_REPO_TOKEN", "--", "true"}, env); code != vm.ExitInterrupted {
				t.Fatalf("exit = %d, want %d; stderr:\n%s", code, vm.ExitInterrupted, stderr())
			}
			if len(*calls) != 0 {
				t.Errorf("the command ran")
			}
			if !strings.HasSuffix(stderr(), tokenConfirmPrompt+vm.ErrInterrupted.Error()+"\n") {
				t.Errorf("stderr:\n%s\nwant the prompt, then %q", stderr(), vm.ErrInterrupted)
			}
		})
	}
}

func TestVMExecEnvOnTerminalStopsOnCtrlC(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	t.Setenv("MY_REPO_TOKEN", "x")
	env, _, stderr := vmTestEnv(t)
	_, done := startVMExecAtPrompt(t, env, stderr)
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := exitCodeWithin(t, done, "Ctrl-C"); code != vm.ExitInterrupted {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, vm.ExitInterrupted, stderr())
	}
	if len(*calls) != 0 {
		t.Errorf("the command ran after Ctrl-C")
	}
	if !strings.HasSuffix(stderr(), tokenConfirmPrompt+"\n"+vm.ErrInterrupted.Error()+"\n") {
		t.Errorf("stderr:\n%s\nwant the prompt, a newline after the ^C echo, then %q", stderr(), vm.ErrInterrupted)
	}
}

// A prompt written to a redirected stderr would wait for a reason nobody sees.
func TestVMExecEnvWithRedirectedStderrDoesNotPrompt(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	t.Setenv("MY_REPO_TOKEN", "x")
	env, _, stderr := vmTestEnv(t)
	useTerminals(t, env.Stdin)
	if code := runVM([]string{"exec", "--env", "MY_REPO_TOKEN", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
	}
	if len(*calls) != 1 || strings.Contains(stderr(), tokenConfirmPrompt) {
		t.Errorf("calls = %d, stderr:\n%s\nwant 1 call and no prompt", len(*calls), stderr())
	}
}

func TestVMExecEnvWithoutTerminalDoesNotPrompt(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	t.Setenv("MY_REPO_TOKEN", "x")
	env, _, stderr := vmTestEnv(t)
	useTerminals(t, env.Stdout, env.Stderr)
	if code := runVM([]string{"exec", "--env", "MY_REPO_TOKEN", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
	}
	if len(*calls) != 1 {
		t.Fatalf("runVMExecFn calls = %d, want 1", len(*calls))
	}
	if !strings.Contains(stderr(), tokenNoticeStart) || strings.Contains(stderr(), tokenConfirmPrompt) {
		t.Errorf("stderr:\n%s\nwant the notice without the prompt", stderr())
	}
}

func TestVMExecWithoutEnvOnTerminalDoesNotPrompt(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	env, _, stderr := vmTestEnv(t)
	useFakeTerminalStdin(t, env, "")
	if code := runVM([]string{"exec", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
	}
	if len(*calls) != 1 || stderr() != "" {
		t.Errorf("calls = %d, stderr = %q; want 1 call and no output", len(*calls), stderr())
	}
}

func TestVMExecWithoutEnvPrintsNoNotice(t *testing.T) {
	useFakeVMExec(t, nil, 0, nil)
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"exec", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
	}
	if stderr() != "" {
		t.Errorf("stderr = %q, want empty", stderr())
	}
}

func TestVMExecEnvRefusedFromSandbox(t *testing.T) {
	for _, spec := range []string{"A=1", "OMAC_TEST_SURELY_UNSET_VAR"} {
		t.Run(spec, func(t *testing.T) {
			calls := useFakeVMExec(t, nil, 0, nil)
			resolveVMConn = func(*Env, string) (vm.ConnInfo, error) {
				return vm.ConnInfo{Target: vm.SSHTarget{User: "agent", Port: 42022}, FromSandbox: true}, nil
			}
			env, _, stderr := vmTestEnv(t)
			if code := runVM([]string{"exec", "--env", spec, "--", "true"}, env); code != ExitMisuse {
				t.Fatalf("exit = %d, want %d; stderr:\n%s", code, ExitMisuse, stderr())
			}
			if !strings.Contains(stderr(), "--env is for priming from the host while the VM is online") {
				t.Errorf("stderr lacks the reason:\n%s", stderr())
			}
			if strings.Contains(stderr(), "not set") {
				t.Errorf("stderr asks for the variable although --env is refused:\n%s", stderr())
			}
			if strings.Contains(stderr(), tokenNoticeStart) {
				t.Errorf("stderr shows the token notice although nothing runs:\n%s", stderr())
			}
			if len(*calls) != 0 {
				t.Errorf("runVMExecFn called despite the refusal")
			}
			// Without --env, exec from the sandbox works.
			if code := runVM([]string{"exec", "--", "true"}, env); code != ExitOK {
				t.Fatalf("exit without --env = %d; stderr:\n%s", code, stderr())
			}
		})
	}
}

func TestVMExecEnvUnreadableStateIsIOError(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	loadVMState = func(string) (*vm.State, error) { return nil, errors.New("state.json: permission denied") }
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"exec", "--env", "A=1", "--", "true"}, env); code != ExitIOError {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, ExitIOError, stderr())
	}
	if !strings.Contains(stderr(), "state.json: permission denied") {
		t.Errorf("stderr lacks the cause:\n%s", stderr())
	}
	if len(*calls) != 0 {
		t.Errorf("runVMExecFn called without knowing the VM's mode")
	}
}

func TestVMExecEnvRefusedUnlessOnline(t *testing.T) {
	for name, st := range map[string]*vm.State{
		"offline":     {Mode: vm.ModeOffline},
		"not created": nil,
	} {
		t.Run(name, func(t *testing.T) {
			calls := useFakeVMExec(t, nil, 0, nil)
			loadVMState = func(string) (*vm.State, error) { return st, nil }
			env, _, stderr := vmTestEnv(t)
			if code := runVM([]string{"exec", "--env", "A=1", "--", "true"}, env); code != ExitMisuse {
				t.Fatalf("exit = %d, want %d; stderr:\n%s", code, ExitMisuse, stderr())
			}
			if !strings.Contains(stderr(), "--env is for priming from the host while the VM is online") {
				t.Errorf("stderr lacks the reason:\n%s", stderr())
			}
			if strings.Contains(stderr(), tokenNoticeStart) {
				t.Errorf("stderr shows the token notice although nothing runs:\n%s", stderr())
			}
			if len(*calls) != 0 {
				t.Errorf("runVMExecFn called despite the refusal")
			}
		})
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
		"no command":     {"exec"},
		"empty after --": {"exec", "--cwd", "x", "--"},
		"cwd no value":   {"exec", "--cwd"},
		"unknown flag":   {"exec", "--bogus", "--", "true"},
		"cwd leaves":     {"exec", "--cwd", "../x", "--", "true"},
		"cwd absolute":   {"exec", "--cwd=/etc", "--", "true"},
		"cwd is --":      {"exec", "--cwd", "--", "true"},
		"cwd is a flag":  {"exec", "--cwd", "-x", "--", "true"},
	} {
		t.Run(name, func(t *testing.T) {
			calls := useFakeVMExec(t, nil, 0, nil)
			resolveVMConn = func(*Env, string) (vm.ConnInfo, error) {
				t.Error("misuse must be caught before the connection is resolved")
				return vm.ConnInfo{}, errors.New("no connection")
			}
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

func TestVMExecEnvMisuse(t *testing.T) {
	for name, tc := range map[string]struct{ spec, want string }{
		"unset":        {"OMAC_TEST_SURELY_UNSET_VAR", "OMAC_TEST_SURELY_UNSET_VAR is not set"},
		"invalid name": {"1BAD=x", "invalid environment variable name"},
	} {
		for _, mode := range []string{vm.ModeOnline, vm.ModeOffline} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				calls := useFakeVMExec(t, nil, 0, nil)
				loadVMState = func(string) (*vm.State, error) { return &vm.State{Mode: mode}, nil }
				env, _, stderr := vmTestEnv(t)
				if code := runVM([]string{"exec", "--env", tc.spec, "--", "true"}, env); code != ExitMisuse {
					t.Fatalf("exit = %d, want %d; stderr:\n%s", code, ExitMisuse, stderr())
				}
				if !strings.Contains(stderr(), tc.want) {
					t.Errorf("stderr lacks %q:\n%s", tc.want, stderr())
				}
				if strings.Contains(stderr(), tokenNoticeStart) {
					t.Errorf("stderr shows the token notice although nothing runs:\n%s", stderr())
				}
				if len(*calls) != 0 {
					t.Fatalf("runVMExecFn called on misuse")
				}
			})
		}
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
	if n := strings.Count(stderr(), "omac vm up"); n != 1 {
		t.Errorf("stderr names `omac vm up` %d times, want once:\n%s", n, stderr())
	}
	if len(*calls) != 0 {
		t.Errorf("runVMExecFn called without a VM")
	}
}

func TestVMExecInterruptedExits130(t *testing.T) {
	useFakeVMExec(t, nil, vm.ExitInterrupted, vm.ErrInterrupted)
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"exec", "--", "sleep", "60"}, env); code != vm.ExitInterrupted {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, vm.ExitInterrupted, stderr())
	}
	if !strings.Contains(stderr(), "interrupted") {
		t.Errorf("stderr lacks the interruption:\n%s", stderr())
	}
}

func TestVMExecNoTTYWhenStdinIsAFile(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	env, _, _ := vmTestEnv(t)
	if code := runVM([]string{"exec", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if (*calls)[0].TTY {
		t.Errorf("TTY = true for a non-terminal stdin")
	}
}

// useTerminals makes isTerminal true for exactly the given files.
func useTerminals(t *testing.T, files ...*os.File) {
	t.Helper()
	orig := isTerminal
	t.Cleanup(func() { isTerminal = orig })
	isTerminal = func(f *os.File) bool { return slices.Contains(files, f) }
}

func TestVMExecTTYOnlyWhenNoStreamIsRedirected(t *testing.T) {
	for name, tc := range map[string]struct {
		terminals func(env *Env) []*os.File
		wantTTY   bool
	}{
		"all terminals":     {func(env *Env) []*os.File { return []*os.File{env.Stdin, env.Stdout, env.Stderr} }, true},
		"stdin redirected":  {func(env *Env) []*os.File { return []*os.File{env.Stdout, env.Stderr} }, false},
		"stdout redirected": {func(env *Env) []*os.File { return []*os.File{env.Stdin, env.Stderr} }, false},
		"stderr redirected": {func(env *Env) []*os.File { return []*os.File{env.Stdin, env.Stdout} }, false},
	} {
		t.Run(name, func(t *testing.T) {
			calls := useFakeVMExec(t, nil, 0, nil)
			env, _, _ := vmTestEnv(t)
			useTerminals(t, tc.terminals(env)...)
			if code := runVM([]string{"exec", "--", "true"}, env); code != ExitOK {
				t.Fatalf("exit = %d", code)
			}
			if got := (*calls)[0].TTY; got != tc.wantTTY {
				t.Errorf("TTY = %v, want %v", got, tc.wantTTY)
			}
		})
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
		for _, want := range []string{"omac vm exec", "--cwd", "--env", "Copies the worktree (the git top level)", "Runs the command", "stay in the VM", "--env works only from", "prints which directory is copied where", "to reach a sibling directory, run from the top level"} {
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

var skillVMCommandRe = regexp.MustCompile("omac vm ([a-z][^`\n]*)")

// The built-in skills tell agents and humans which `omac vm` commands to run,
// so a renamed verb or flag must fail here rather than in their terminal.
func TestBuiltinSkillVMCommandsParse(t *testing.T) {
	checkVMCommandsIn(t, os.DirFS("../builtinskills/assets"))
}

// The docs show `omac vm` commands to copy, too, in code blocks and inline.
func TestDocsVMCommandsParse(t *testing.T) {
	checkVMCommandsIn(t, os.DirFS("../../docs"))
}

// checkVMCommandsIn checks every `omac vm` command on every line of the
// Markdown files in dir. A trailing " # comment", as in a code block, is
// not part of the command.
func checkVMCommandsIn(t *testing.T, dir fs.FS) {
	t.Helper()
	found := 0
	err := fs.WalkDir(dir, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
			return err
		}
		text, err := fs.ReadFile(dir, path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(text), "\n") {
			if i := strings.Index(line, " #"); i >= 0 {
				line = line[:i]
			}
			for _, m := range skillVMCommandRe.FindAllStringSubmatch(line, -1) {
				if checkVMCommand(t, path, m[1]) {
					found++
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("found no `omac vm` commands")
	}
}

// vmVerbMentions are verbs that prose names without the arguments they
// need.
var vmVerbMentions = []string{"exec", "image add", "image rm"}

// checkVMCommand fails the test when `omac vm <cmdline>` from path would not
// run; it reports whether cmdline was a command rather than a mention of
// a verb. exec runs through the dispatcher with a fake copy and VM.
func checkVMCommand(t *testing.T, path, cmdline string) bool {
	t.Helper()
	args := strings.Fields(cmdline)
	if slices.Contains(vmVerbMentions, strings.Join(args, " ")) {
		return false
	}
	for i, a := range args {
		if strings.HasPrefix(a, "<") && strings.HasSuffix(a, ">") {
			args[i] = "placeholder" // such as <image>, which must parse as an image reference
		}
	}
	if args[0] == "exec" {
		calls := useFakeVMExec(t, nil, 0, nil)
		for i, a := range args {
			spec, ok := strings.CutPrefix(a, "--env=")
			if !ok && a == "--env" && i+1 < len(args) {
				spec, ok = args[i+1], true
			}
			if name, _, hasValue := strings.Cut(spec, "="); ok && !hasValue {
				t.Setenv(name, "x")
			}
		}
		env, _, stderr := vmTestEnv(t)
		if code := runVM(args, env); code != ExitOK || len(*calls) != 1 {
			t.Errorf("%s: omac vm %s exits %d without running\n%s", path, cmdline, code, stderr())
		}
		return true
	}
	useFakeVM(t, &fakeVMManager{})
	env, _, stderr := vmTestEnv(t)
	if code := runVM(args, env); code != ExitOK {
		t.Errorf("%s: omac vm %s exits %d\n%s", path, cmdline, code, stderr())
	}
	return true
}

// The skill quotes omac's exit codes, so the agent can tell them from the
// command's own.
func TestOmacVMSkillNamesExitCodes(t *testing.T) {
	text, err := os.ReadFile("../builtinskills/assets/omac-vm/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("(%d, %d or %d)", ExitMisuse, ExitPrerequisiteMissing, ExitIOError); !strings.Contains(string(text), want) {
		t.Errorf("SKILL.md does not name omac's exit codes as %q", want)
	}
}

func TestOmacVMSkillNamesSessionVariable(t *testing.T) {
	text, err := os.ReadFile("../builtinskills/assets/omac-vm/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), vm.EnvVMSSHPort) {
		t.Errorf("SKILL.md does not mention %s", vm.EnvVMSSHPort)
	}
}
