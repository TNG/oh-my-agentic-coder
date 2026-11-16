package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/vm"
)

// useNamedFakeVM is useFakeVM that also records the VM name each manager
// was built for.
func useNamedFakeVM(t *testing.T, fake *fakeVMManager) *[]string {
	t.Helper()
	var names []string
	orig := newVMManager
	newVMManager = func(_ *Env, name string) (vmManager, error) {
		names = append(names, name)
		return fake, nil
	}
	t.Cleanup(func() { newVMManager = orig })
	return &names
}

func TestVMNameSelectsInstance(t *testing.T) {
	cases := []struct {
		args     []string
		wantName string
		wantCall string
	}{
		{[]string{"up"}, "omac", "Up(false)"},
		{[]string{"up", "--name", "work"}, "work", "Up(false)"},
		{[]string{"up", "--reprovision", "--name=work"}, "work", "Up(true)"},
		{[]string{"up", "--name", "work", "--reprovision"}, "work", "Up(true)"},
		{[]string{"down", "--name", "work"}, "work", "Down"},
		{[]string{"reset", "--name", "work"}, "work", "Reset"},
		{[]string{"status", "--name", "work"}, "work", "Status"},
		{[]string{"online", "--name", "work"}, "work", "SetMode(online)"},
		{[]string{"offline", "--name", "work"}, "work", "SetMode(offline)"},
		{[]string{"image", "list", "--name", "work"}, "work", "ImageList"},
		{[]string{"image", "add", "alpine", "--name", "work"}, "work", "ImageAdd(alpine)"},
		{[]string{"image", "rm", "x", "--name=work"}, "work", "ImageRemove(x)"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			fake := &fakeVMManager{}
			names := useNamedFakeVM(t, fake)
			env, _, stderr := vmTestEnv(t)
			if code := runVM(tc.args, env); code != ExitOK {
				t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
			}
			if !reflect.DeepEqual(*names, []string{tc.wantName}) {
				t.Errorf("manager names = %v, want [%s]", *names, tc.wantName)
			}
			if !reflect.DeepEqual(fake.calls, []string{tc.wantCall}) {
				t.Errorf("calls = %v, want [%s]", fake.calls, tc.wantCall)
			}
		})
	}
}

func TestVMInvalidNameIsMisuse(t *testing.T) {
	for _, bad := range []string{"", "Work", "1x", "a_b", "../x", "abcdefghijklmnopq"} {
		for _, verb := range []string{"up", "down", "reset", "status", "online", "offline"} {
			fake := &fakeVMManager{}
			names := useNamedFakeVM(t, fake)
			env, _, stderr := vmTestEnv(t)
			if code := runVM([]string{verb, "--name", bad}, env); code != ExitMisuse {
				t.Errorf("%s --name %q: exit = %d, want %d", verb, bad, code, ExitMisuse)
			}
			if !strings.Contains(stderr(), "invalid VM name") {
				t.Errorf("%s --name %q: stderr lacks the reason:\n%s", verb, bad, stderr())
			}
			if len(*names) != 0 {
				t.Errorf("%s --name %q built a manager", verb, bad)
			}
		}
	}
}

func TestVMNameWithoutValueIsMisuse(t *testing.T) {
	names := useNamedFakeVM(t, &fakeVMManager{})
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"up", "--name"}, env); code != ExitMisuse {
		t.Fatalf("exit = %d, want %d", code, ExitMisuse)
	}
	if !strings.Contains(stderr(), "--name needs a value") || len(*names) != 0 {
		t.Errorf("stderr = %q, names = %v", stderr(), *names)
	}
}

func TestVMExecNamePicksInstanceOutsideSession(t *testing.T) {
	useFakeVMExec(t, nil, 0, nil)
	var connName, stateName string
	resolveVMConn = func(_ *Env, name string) (vm.ConnInfo, error) {
		connName = name
		return vm.ConnInfo{Target: vm.SSHTarget{User: "agent", Port: 1}}, nil
	}
	loadVMState = func(name string) (*vm.State, error) {
		stateName = name
		return &vm.State{Mode: vm.ModeOnline}, nil
	}
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"exec", "--name", "work", "--env", "A=1", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
	}
	if connName != "work" || stateName != "work" {
		t.Errorf("conn name = %q, state name = %q, want work for both", connName, stateName)
	}
	if code := runVM([]string{"exec", "--", "true"}, env); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if connName != "omac" {
		t.Errorf("default conn name = %q, want omac", connName)
	}
}

func TestVMExecInvalidNameIsMisuse(t *testing.T) {
	calls := useFakeVMExec(t, nil, 0, nil)
	resolveVMConn = func(*Env, string) (vm.ConnInfo, error) {
		t.Error("an invalid name must be caught before the connection is resolved")
		return vm.ConnInfo{}, nil
	}
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"exec", "--name", "Bad", "--", "true"}, env); code != ExitMisuse {
		t.Fatalf("exit = %d, want %d", code, ExitMisuse)
	}
	if !strings.Contains(stderr(), "invalid VM name") || len(*calls) != 0 {
		t.Errorf("stderr = %q, calls = %d", stderr(), len(*calls))
	}
}

func TestVMExecOnlineHintNamesTheInstance(t *testing.T) {
	useFakeVMExec(t, nil, 0, nil)
	loadVMState = func(string) (*vm.State, error) { return &vm.State{Mode: vm.ModeOffline}, nil }
	env, _, stderr := vmTestEnv(t)
	runVM([]string{"exec", "--name", "work", "--env", "A=1", "--", "true"}, env)
	if !strings.Contains(stderr(), "omac vm online --name work") {
		t.Errorf("hint lacks the instance:\n%s", stderr())
	}
}

func TestVMListShowsEveryInstance(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	for name, st := range map[string]*vm.State{
		"omac": {SSHPort: 42022, Mode: vm.ModeOffline},
		"work": {SSHPort: 42023, Mode: vm.ModeOnline},
	} {
		root, err := vm.InstanceRoot(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := vm.SaveState(filepath.Join(root, "state.json"), st); err != nil {
			t.Fatal(err)
		}
	}
	env, stdout, stderr := vmTestEnv(t)
	if code := runVM([]string{"list"}, env); code != ExitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
	}
	got := strings.Fields(stdout())
	want := []string{"NAME", "STATE", "SSH", "PORT", "omac", "offline", "42022", "work", "online", "42023"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("list output fields = %q, want %q", got, want)
	}
}

func TestVMListRejectsArguments(t *testing.T) {
	for _, args := range [][]string{{"list", "x"}, {"list", "--name", "work"}} {
		env, _, _ := vmTestEnv(t)
		if code := runVM(args, env); code != ExitMisuse {
			t.Errorf("%v: exit = %d, want %d", args, code, ExitMisuse)
		}
	}
}
