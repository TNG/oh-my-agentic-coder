package cli

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/TNG/oh-my-agentic-coder/internal/vm"
)

// fakeVMManager records every call; err (if set) is returned by all methods.
type fakeVMManager struct {
	factoryCalls int
	calls        []string
	err          error
	report       vm.StatusReport
	images       []string
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
	newVMManager = func(*Env, string) (vmManager, error) { fake.factoryCalls++; return fake, nil }
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
		verbs := dispatchedVMVerbs(t)
		for verb := range vmNoArgOps {
			verbs = append(verbs, verb)
		}
		for _, verb := range verbs {
			if !strings.Contains(out, "omac vm "+verb+" ") {
				t.Errorf("%s: usage lacks %q:\n%s", arg, verb, out)
			}
		}
		for _, form := range vmUsageArgumentForms {
			if !strings.Contains(out, "omac vm "+form) {
				t.Errorf("%s: usage lacks %q:\n%s", arg, form, out)
			}
		}
	}
}

// vmUsageArgumentForms are arguments the help must show. dispatchedVMVerbs
// reads only case labels, not the argument parsing in the case bodies.
var vmUsageArgumentForms = []string{"up [--reprovision]", "image add <ref>", "image rm <ref>"}

// dispatchedVMVerbs reads the verbs from the switch statements in
// vm_cmd.go, so a verb added there without a usage line fails the help
// test. A switch on `verb` holds top-level verbs, a switch on `sub` the
// image verbs.
func dispatchedVMVerbs(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "vm_cmd.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	prefix := map[string]string{"verb": "", "sub": "image "}
	matched := map[string]bool{}
	var verbs []string
	ast.Inspect(file, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		tag, ok := sw.Tag.(*ast.Ident)
		if !ok {
			return true
		}
		p, ok := prefix[tag.Name]
		if !ok {
			return true
		}
		matched[tag.Name] = true
		for _, stmt := range sw.Body.List {
			for _, expr := range stmt.(*ast.CaseClause).List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				if v != "help" && !strings.HasPrefix(v, "-") {
					verbs = append(verbs, p+v)
				}
			}
		}
		return true
	})
	for name := range prefix {
		if !matched[name] {
			t.Fatalf("found no switch on %q in vm_cmd.go; update dispatchedVMVerbs", name)
		}
	}
	return verbs
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
		{"image", "rm", "a", "b"},
		{"image", "add", "Bad Ref"},
		{"image", "add", "alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"image", "rm", "Bad Ref"},
		{"image", "list", "extra"},
		{"image", "bogus"},
		{"bogus"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			fake := &fakeVMManager{}
			useFakeVM(t, fake)
			env, _, stderr := vmTestEnv(t)
			if code := runVM(args, env); code != ExitMisuse {
				t.Fatalf("exit = %d, want %d", code, ExitMisuse)
			}
			if fake.factoryCalls != 0 {
				t.Fatalf("manager built %d times, want 0: misuse must be caught before the VM is touched", fake.factoryCalls)
			}
			if out := stderr(); !strings.Contains(out, "omac vm: ") || !strings.Contains(out, "Usage:") {
				t.Fatalf("stderr lacks the error line or usage:\n%s", out)
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
		{fmt.Errorf("%w: exec: not found", vm.ErrLimaMissing), ExitPrerequisiteMissing, "lima-vm.io/docs/installation"},
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

func TestVMReadVerbErrorPrintsNothingToStdout(t *testing.T) {
	for _, args := range [][]string{{"status"}, {"image", "list"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			useFakeVM(t, &fakeVMManager{err: errors.New("boom"), images: []string{"alpine:3"}})
			env, stdout, stderr := vmTestEnv(t)
			if code := runVM(args, env); code != ExitGeneric {
				t.Fatalf("exit = %d, want %d", code, ExitGeneric)
			}
			if out := stdout(); out != "" {
				t.Fatalf("stdout = %q, want empty", out)
			}
			if !strings.Contains(stderr(), "boom") {
				t.Fatalf("stderr lacks cause:\n%s", stderr())
			}
		})
	}
}

func TestVMManagerConstructionFailureIsIOError(t *testing.T) {
	orig := newVMManager
	newVMManager = func(*Env, string) (vmManager, error) { return nil, errors.New("no home") }
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

// The top-level help names the vm verbs by hand; every verb `omac vm help`
// shows must be in it.
func TestTopLevelHelpListsEveryVMVerb(t *testing.T) {
	useFakeVM(t, &fakeVMManager{})
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"help"}, env); code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	verbs := map[string]bool{}
	for _, line := range strings.Split(stderr(), "\n") {
		if rest, ok := strings.CutPrefix(line, "  omac vm "); ok {
			verbs[strings.Fields(rest)[0]] = true
		}
	}
	if len(verbs) == 0 {
		t.Fatal("omac vm help lists no verb")
	}
	f, err := os.CreateTemp(t.TempDir(), "usage-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	printUsage(f)
	usage, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	short := commands()["vm"].Short
	var vmLine string
	for _, line := range strings.Split(string(usage), "\n") {
		if strings.HasPrefix(line, "  vm ") {
			vmLine = line
		}
	}
	for verb := range verbs {
		listed := regexp.MustCompile(`[ :|]` + regexp.QuoteMeta(verb) + `[|.]`)
		if !listed.MatchString(short) {
			t.Errorf("commands()[\"vm\"].Short lacks %q: %s", verb, short)
		}
		if !listed.MatchString(vmLine) {
			t.Errorf("printUsage vm line lacks %q: %s", verb, vmLine)
		}
	}
}

// vmHelpWidth is the widest line `omac vm help` may print.
const vmHelpWidth = 115

func TestVMHelpLinesStayNarrow(t *testing.T) {
	useFakeVM(t, &fakeVMManager{})
	env, _, stderr := vmTestEnv(t)
	if code := runVM([]string{"help"}, env); code != ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, line := range strings.Split(stderr(), "\n") {
		if n := utf8.RuneCountInString(line); n > vmHelpWidth {
			t.Errorf("help line has %d characters, more than %d:\n%s", n, vmHelpWidth, line)
		}
	}
}
