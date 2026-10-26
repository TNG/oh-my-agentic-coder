package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/vm"
)

// fakeVMManager records every call; err (if set) is returned by all methods.
type fakeVMManager struct {
	calls  []string
	err    error
	report vm.StatusReport
	images []string
}

func (f *fakeVMManager) record(s string) error { f.calls = append(f.calls, s); return f.err }

func (f *fakeVMManager) Up(_ context.Context, reprovision bool) error {
	return f.record(fmt.Sprintf("Up(%v)", reprovision))
}
func (f *fakeVMManager) Down(context.Context) error  { return f.record("Down") }
func (f *fakeVMManager) Reset(context.Context) error { return f.record("Reset") }
func (f *fakeVMManager) Status(context.Context) (vm.StatusReport, error) {
	return f.report, f.record("Status")
}
func (f *fakeVMManager) SetMode(_ context.Context, mode string) error {
	return f.record("SetMode(" + mode + ")")
}
func (f *fakeVMManager) ImageAdd(_ context.Context, ref string) error {
	return f.record("ImageAdd(" + ref + ")")
}
func (f *fakeVMManager) ImageList(context.Context) ([]string, error) {
	return f.images, f.record("ImageList")
}
func (f *fakeVMManager) ImageRemove(_ context.Context, ref string) error {
	return f.record("ImageRemove(" + ref + ")")
}
func (f *fakeVMManager) PrepareAttach(context.Context) (vm.AttachInfo, error) {
	return vm.AttachInfo{}, f.record("PrepareAttach")
}

// useFakeVM swaps the newVMManager seam for the duration of the test.
func useFakeVM(t *testing.T, fake *fakeVMManager) {
	t.Helper()
	orig := newVMManager
	newVMManager = func(*Env) (vmManager, error) { return fake, nil }
	t.Cleanup(func() { newVMManager = orig })
}

// vmTestEnv returns an Env with temp-file stdout/stderr and readers for both.
func vmTestEnv(t *testing.T) (*Env, func() string, func() string) {
	t.Helper()
	env, stderr := launchTestEnv(t, t.TempDir())
	stdout := func() string {
		t.Helper()
		if err := env.Stdout.Sync(); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(env.Stdout.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	return env, stdout, stderr
}

func TestVMNoVerbIsMisuse(t *testing.T) {
	fake := &fakeVMManager{}
	useFakeVM(t, fake)
	env, _, stderr := vmTestEnv(t)
	if code := runVM(nil, env); code != ExitMisuse {
		t.Fatalf("exit = %d, want %d", code, ExitMisuse)
	}
	if !strings.Contains(stderr(), "Usage:") {
		t.Fatalf("stderr lacks usage:\n%s", stderr())
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %v, want none", fake.calls)
	}
}

func TestVMHelpListsEveryVerb(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		useFakeVM(t, &fakeVMManager{})
		env, _, stderr := vmTestEnv(t)
		if code := runVM([]string{arg}, env); code != ExitOK {
			t.Fatalf("%s: exit = %d, want 0", arg, code)
		}
		out := stderr()
		for _, verb := range []string{"up [--reprovision]", "down", "reset", "status", "online", "offline", "image add <ref>", "image list", "image rm <ref>"} {
			if !strings.Contains(out, "omac vm "+verb) {
				t.Errorf("%s: usage lacks %q:\n%s", arg, verb, out)
			}
		}
		if strings.Contains(out, "exec") {
			t.Errorf("%s: usage mentions exec, reserved for a later release:\n%s", arg, out)
		}
	}
}

func TestVMDispatch(t *testing.T) {
	cases := []struct {
		args      []string
		wantCalls []string
	}{
		{[]string{"up"}, []string{"Up(false)"}},
		{[]string{"up", "--reprovision"}, []string{"Up(true)"}},
		{[]string{"down"}, []string{"Down"}},
		{[]string{"reset"}, []string{"Reset"}},
		{[]string{"status"}, []string{"Status"}},
		{[]string{"online"}, []string{"SetMode(online)"}},
		{[]string{"offline"}, []string{"SetMode(offline)"}},
		{[]string{"image", "add", "alpine"}, []string{"ImageAdd(alpine)"}},
		{[]string{"image", "list"}, []string{"ImageList"}},
		{[]string{"image", "rm", "x"}, []string{"ImageRemove(x)"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			fake := &fakeVMManager{}
			useFakeVM(t, fake)
			env, _, stderr := vmTestEnv(t)
			if code := runVM(tc.args, env); code != ExitOK {
				t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr())
			}
			if !reflect.DeepEqual(fake.calls, tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", fake.calls, tc.wantCalls)
			}
		})
	}
}

func TestVMMisuse(t *testing.T) {
	cases := [][]string{
		{"up", "--bogus"},
		{"up", "extra"},
		{"down", "extra"},
		{"reset", "extra"},
		{"status", "extra"},
		{"online", "extra"},
		{"offline", "extra"},
		{"image"},
		{"image", "add"},
		{"image", "add", "a", "b"},
		{"image", "rm"},
		{"image", "list", "extra"},
		{"image", "bogus"},
		{"bogus"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			fake := &fakeVMManager{}
			useFakeVM(t, fake)
			env, _, _ := vmTestEnv(t)
			if code := runVM(args, env); code != ExitMisuse {
				t.Fatalf("exit = %d, want %d", code, ExitMisuse)
			}
			if len(fake.calls) != 0 {
				t.Fatalf("calls = %v, want none", fake.calls)
			}
		})
	}
}

func TestVMStatusPrintsReport(t *testing.T) {
	fake := &fakeVMManager{report: vm.StatusReport{Created: true, Running: true, Mode: "offline", SSHPort: 60022}}
	useFakeVM(t, fake)
	env, stdout, _ := vmTestEnv(t)
	if code := runVM([]string{"status"}, env); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	out := stdout()
	for _, want := range []string{"status: running", "mode: offline", "ssh: 127.0.0.1:60022"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
}

func TestVMImageListPrintsOneRefPerLine(t *testing.T) {
	fake := &fakeVMManager{images: []string{"alpine:3", "postgres:16"}}
	useFakeVM(t, fake)
	env, stdout, _ := vmTestEnv(t)
	if code := runVM([]string{"image", "list"}, env); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if got, want := stdout(), "alpine:3\npostgres:16\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestVMErrorMapping(t *testing.T) {
	cases := []struct {
		err        error
		want       int
		wantStderr string
	}{
		{fmt.Errorf("%w: exec: not found", vm.ErrLimaMissing), ExitPrerequisiteMissing, "brew install lima"},
		{vm.ErrNotCreated, ExitMisuse, "omac vm up"},
		{vm.ErrNotRunning, ExitMisuse, "not running"},
		{vm.ErrOnline, ExitMisuse, "omac vm offline"},
		{fmt.Errorf("%w (pids 1, 2)", vm.ErrSessionsAttached), ExitMisuse, "sessions are attached"},
		{vm.ErrFirewallMissing, ExitMisuse, "omac-vmguard"},
		{errors.New("boom"), ExitGeneric, "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.err.Error(), func(t *testing.T) {
			useFakeVM(t, &fakeVMManager{err: tc.err})
			env, _, stderr := vmTestEnv(t)
			if code := runVM([]string{"down"}, env); code != tc.want {
				t.Fatalf("exit = %d, want %d", code, tc.want)
			}
			if !strings.Contains(stderr(), tc.wantStderr) {
				t.Fatalf("stderr lacks %q:\n%s", tc.wantStderr, stderr())
			}
		})
	}
}

func TestVMManagerConstructionFailureIsIOError(t *testing.T) {
	orig := newVMManager
	newVMManager = func(*Env) (vmManager, error) { return nil, errors.New("no home") }
	t.Cleanup(func() { newVMManager = orig })
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"status"}, env); code != ExitIOError {
		t.Fatalf("exit = %d, want %d", code, ExitIOError)
	}
	if !strings.Contains(stderr(), "no home") {
		t.Fatalf("stderr lacks cause:\n%s", stderr())
	}
}

func TestCommandsRegistersVM(t *testing.T) {
	if _, ok := commands()["vm"]; !ok {
		t.Fatal(`commands() has no "vm" entry`)
	}
	f, err := os.CreateTemp(t.TempDir(), "usage-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	printUsage(f)
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "  vm ") {
		t.Fatalf("printUsage lacks vm line:\n%s", b)
	}
}
