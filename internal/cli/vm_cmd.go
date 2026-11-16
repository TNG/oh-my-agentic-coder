package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/TNG/oh-my-agentic-coder/internal/vm"
)

// vmManager is the subset of *vm.Manager the CLI needs; tests inject a fake.
type vmManager interface {
	Up(ctx context.Context, reprovision bool) error
	Down(ctx context.Context) error
	Reset(ctx context.Context) error
	Status(ctx context.Context) (vm.StatusReport, error)
	SetMode(ctx context.Context, mode string) error
	ImageAdd(ctx context.Context, ref string) error
	ImageList(ctx context.Context) ([]string, error)
	ImageRemove(ctx context.Context, ref string) error
	PrepareAttach(ctx context.Context) (vm.AttachInfo, error)
}

// newVMManager builds the real manager. Tests replace it so no VM is touched.
var newVMManager = func(env *Env, name string) (vmManager, error) {
	root, err := vm.InstanceRoot(name)
	if err != nil {
		return nil, err
	}
	return vm.NewManager(vm.NewLayout(root), vm.InstanceConfigPath(name), vm.RealDeps(), env.Stdout), nil
}

// extractVMName removes --name <x> or --name=<x> from args wherever it sits
// and returns its value; named is false when the flag is absent. The
// arguments left over keep their order.
func extractVMName(args []string) (name string, named bool, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--name":
			if i+1 >= len(args) {
				return "", false, nil, errors.New("--name needs a value")
			}
			name, named = args[i+1], true
			i++
		case strings.HasPrefix(a, "--name="):
			name, named = strings.TrimPrefix(a, "--name="), true
		default:
			rest = append(rest, a)
		}
	}
	return name, named, rest, nil
}

// vmOp is one validated `omac vm` invocation, run once the manager exists.
type vmOp func(ctx context.Context, m vmManager, env *Env) error

// runVM dispatches `omac vm <verb>`. Arguments are validated before the
// manager is built, so misuse never touches the VM.
func runVM(args []string, env *Env) int {
	if len(args) == 0 {
		printVMUsage(env)
		return ExitMisuse
	}
	verb, rest := args[0], args[1:]
	name := vm.DefaultVMName
	if verb != "exec" && verb != "help" && verb != "-h" && verb != "--help" {
		given, named, left, err := extractVMName(rest)
		if err != nil {
			return vmMisuse(env, err.Error())
		}
		if named {
			if verb == "list" {
				return vmMisuse(env, "list takes no arguments")
			}
			if err := vm.ValidateName(given); err != nil {
				return vmMisuse(env, err.Error())
			}
			name = given
		}
		rest = left
	}
	var op vmOp
	switch verb {
	case "list":
		if len(rest) > 0 {
			return vmMisuse(env, "list takes no arguments")
		}
		rows, err := vm.ListInstances()
		if err != nil {
			fmt.Fprintln(env.Stderr, err)
			return ExitIOError
		}
		writeVMList(env.Stdout, rows)
		return ExitOK
	case "up":
		reprovision := false
		for _, a := range rest {
			if a != "--reprovision" {
				return vmMisuse(env, fmt.Sprintf("up: unexpected argument %q", a))
			}
			reprovision = true
		}
		op = func(ctx context.Context, m vmManager, _ *Env) error { return m.Up(ctx, reprovision) }
	case "image":
		var code int
		if op, code = parseVMImage(rest, env); op == nil {
			return code
		}
	case "exec":
		// exec talks to the VM over ssh only, so it never builds a manager.
		return runVMExec(rest, env)
	case "help", "-h", "--help":
		printVMUsage(env)
		return ExitOK
	default:
		var known bool
		if op, known = vmNoArgOps[verb]; !known {
			return vmMisuse(env, fmt.Sprintf("unknown verb %q", verb))
		}
		if len(rest) > 0 {
			return vmMisuse(env, fmt.Sprintf("%s takes no arguments", verb))
		}
	}

	// Errors print as returned: most Manager errors already start with
	// "omac vm", and a prefix here would repeat it.
	m, err := newVMManager(env, name)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return ExitIOError
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := op(ctx, m, env); err != nil {
		fmt.Fprintln(env.Stderr, err)
		if errors.Is(err, vm.ErrLimaMissing) {
			fmt.Fprintln(env.Stderr, "omac vm: install Lima first, see https://lima-vm.io/docs/installation/")
		}
		return vmExitCode(err)
	}
	return ExitOK
}

// resolveVMConn finds the VM's ssh connection: from the OMAC_VM_*
// variables inside a sandbox, else from the host-side state directory.
var resolveVMConn = func(_ *Env, name string) (vm.ConnInfo, error) {
	root, err := vm.InstanceRoot(name)
	if err != nil {
		return vm.ConnInfo{}, err
	}
	return vm.ResolveConn(os.Getenv, vm.NewLayout(root), vm.RealDeps().HostUser)
}

// isTerminal reports whether f is a terminal. Tests replace it.
var isTerminal = func(f *os.File) bool { return f != nil && term.IsTerminal(int(f.Fd())) }

// loadVMState reads the host-side state.json; nil when the VM was never
// created. Tests replace it.
var loadVMState = func(name string) (*vm.State, error) {
	root, err := vm.InstanceRoot(name)
	if err != nil {
		return nil, err
	}
	return vm.LoadState(vm.NewLayout(root).StatePath)
}

// runVMExecFn is vm.Exec; tests replace it so no VM is touched.
var runVMExecFn = vm.Exec

// vmExecArgs is a parsed `omac vm exec` invocation.
type vmExecArgs struct {
	name    string
	cwd     string
	envs    []string
	cmd     []string
	verbose bool
}

// parseVMExecArgs parses [--name <x>] [--cwd <dir>] [--env SPEC]... [--verbose] [--] <cmd...>.
// Flags end at "--" or at the first argument that is not a flag, so the
// command keeps its own flags. ok is false when the caller should exit with
// code: after -h/--help or a usage error.
func parseVMExecArgs(args []string, env *Env) (a vmExecArgs, code int, ok bool) {
	fs := flag.NewFlagSet("omac vm exec", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.name, "name", vm.DefaultVMName, "")
	fs.StringVar(&a.cwd, "cwd", "", "")
	fs.Func("env", "", func(s string) error {
		a.envs = append(a.envs, s)
		return nil
	})
	fs.BoolVar(&a.verbose, "verbose", false, "")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		printVMExecUsage(env.Stdout)
		return a, ExitOK, false
	} else if err != nil {
		return a, vmExecMisuse(env, err.Error()), false
	}
	if a.cmd = fs.Args(); len(a.cmd) == 0 {
		return a, vmExecMisuse(env, "no command given"), false
	}
	if err := vm.ValidateName(a.name); err != nil {
		return a, vmExecMisuse(env, err.Error()), false
	}
	if err := vm.ValidateCwd(a.cwd); err != nil {
		return a, vmExecMisuseErr(env, err), false
	}
	return a, ExitOK, true
}

// runVMExec runs `omac vm exec`: copy the worktree into the VM, run the
// command there, return its exit code.
func runVMExec(args []string, env *Env) int {
	a, code, ok := parseVMExecArgs(args, env)
	if !ok {
		return code
	}
	conn, err := resolveVMConn(env, a.name)
	if err != nil {
		fmt.Fprintln(env.Stderr, err) // ErrNotCreated already says to run `omac vm up`
		if errors.Is(err, vm.ErrNotCreated) {
			return ExitPrerequisiteMissing
		}
		return ExitIOError
	}
	envs, code, ok := resolveVMExecEnv(env, conn, a.envs, a.name)
	if !ok {
		return code
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	printVMExecTokenNotice(env.Stderr, envNames(envs))
	// Nothing is copied or sent before this point, so the human can still
	// stop and run `omac vm reset` as the notice says.
	if len(envs) > 0 && isTerminal(env.Stdin) && isTerminal(env.Stderr) {
		fmt.Fprintln(env.Stderr, vmExecEnvPrompt)
		if !waitForEnter(ctx, env.Stdin) {
			if ctx.Err() != nil {
				fmt.Fprintln(env.Stderr) // finish the line a ^C echo may have left open
			}
			fmt.Fprintln(env.Stderr, vm.ErrInterrupted)
			return vm.ExitInterrupted
		}
		// The wait can be long; a session may have attached meanwhile.
		if err := checkVMOnline(a.name); err != nil {
			fmt.Fprintln(env.Stderr, err)
			if errors.Is(err, errVMNotOnline) {
				return ExitMisuse
			}
			return ExitIOError
		}
	}
	var logf func(string, ...any)
	if a.verbose {
		logf = func(format string, v ...any) { fmt.Fprintf(env.Stderr, format+"\n", v...) }
	}
	code, err = runVMExecFn(ctx, vm.ExecOpts{
		Workdir: env.Workdir,
		Cwd:     a.cwd,
		Env:     envs,
		Cmd:     a.cmd,
		Conn:    conn,
		Run:     vm.ExecRunner,
		SSH:     vm.DefaultSSH,
		// A terminal in the VM turns "\n" into "\r\n" and merges stderr into
		// stdout, so a redirected stream such as `2>build.log` rules it out.
		TTY:    isTerminal(env.Stdin) && isTerminal(env.Stdout) && isTerminal(env.Stderr),
		Stdin:  env.Stdin,
		Stdout: env.Stdout,
		Stderr: env.Stderr,
		Log:    logf,
	})
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		if errors.Is(err, vm.ErrInterrupted) {
			return code
		}
		return ExitIOError
	}
	return code
}

// resolveVMExecEnv limits --env to priming: from the host, while the VM is
// online, which also means no session is attached. A sandbox is refused
// before any lookup, since it filters most variables and "not set" would
// send the agent looking for the secret; on the host a bad --env is
// reported before the user brings the VM online for nothing.
func resolveVMExecEnv(env *Env, conn vm.ConnInfo, specs []string, name string) ([]string, int, bool) {
	if len(specs) == 0 {
		return nil, ExitOK, true
	}
	if conn.FromSandbox {
		return nil, vmExecMisuse(env, vmExecEnvReason), false
	}
	envs, err := vm.ResolveEnv(specs, os.LookupEnv)
	if err != nil {
		return nil, vmExecMisuseErr(env, err), false
	}
	if err := checkVMOnline(name); errors.Is(err, errVMNotOnline) {
		return nil, vmExecMisuseErr(env, err), false
	} else if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return nil, ExitIOError, false
	}
	return envs, ExitOK, true
}

const vmExecEnvReason = "--env is for priming from the host while the VM is online"

// vmExecEnvPrompt asks for consent before --env sends a token. A shell
// without bracketed paste hands the next pasted line to this prompt.
const vmExecEnvPrompt = "Press Enter to continue, Ctrl-C to stop.\n" +
	"A line pasted after this command answers this prompt: omac stops, and the pasted line does not run."

var errVMNotOnline = errors.New("omac vm exec: " + vmExecEnvReason)

// checkVMOnline returns an error wrapping errVMNotOnline, or the error
// reading the VM state.
func checkVMOnline(name string) error {
	st, err := loadVMState(name)
	if err != nil {
		return err
	}
	if st == nil || st.Mode != vm.ModeOnline {
		return fmt.Errorf("%w; run `omac vm online%s` first", errVMNotOnline, vmNameSuffix(name))
	}
	return nil
}

// envNames returns the distinct names in envs (NAME=VALUE), in order.
func envNames(envs []string) []string {
	var names []string
	for _, kv := range envs {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

func printVMExecTokenNotice(w io.Writer, names []string) {
	if len(names) == 0 {
		return
	}
	list, tokens := names[0], "this token"
	if n := len(names); n > 1 {
		list = strings.Join(names[:n-1], ", ") + " and " + names[n-1]
		tokens = "these tokens"
	}
	fmt.Fprintf(w, "omac vm exec: the build in the VM can read %s.\n"+
		"If a session you do not trust used the VM since your last `omac vm reset`, stop and run it first:\n"+
		"files that session left in the VM run with %s.\n", list, tokens)
}

// waitForEnter reports whether the first line read from r is empty, so a
// typed "n" or a pasted next command does not count as consent. It is false
// at end of input or when ctx ends. It reads one byte at a time, so the
// command run afterwards gets the rest of r.
func waitForEnter(ctx context.Context, r io.Reader) bool {
	entered := make(chan bool, 1)
	go func() {
		b := make([]byte, 1)
		for empty := true; ; empty = false {
			if _, err := r.Read(b); err != nil {
				entered <- false
				return
			}
			if b[0] == '\n' {
				entered <- empty
				return
			}
		}
	}()
	select {
	case ok := <-entered:
		return ok
	case <-ctx.Done():
		return false
	}
}

func vmExecMisuse(env *Env, msg string) int {
	return vmExecMisuseErr(env, errors.New("omac vm exec: "+msg))
}

// vmExecMisuseErr prints err as returned, since vm errors already start
// with "omac vm exec: ".
func vmExecMisuseErr(env *Env, err error) int {
	fmt.Fprintln(env.Stderr, err)
	printVMExecUsage(env.Stderr)
	return ExitMisuse
}

// vmNameSuffix is the " --name <x>" a hint needs for any VM but the default.
func vmNameSuffix(name string) string {
	if name == vm.DefaultVMName {
		return ""
	}
	return " --name " + name
}

func writeVMList(w io.Writer, rows []vm.InstanceRow) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tSSH PORT\tPORTS")
	for _, r := range rows {
		port, ports := "-", "-"
		if r.SSHPort > 0 {
			port = strconv.Itoa(r.SSHPort)
			first, last := vm.PortBlockRange(r.PortBlock)
			ports = fmt.Sprintf("%d-%d", first, last)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Mode, port, ports)
	}
	tw.Flush()
}

const vmExecUsage = `  omac vm exec [--name <x>] [--cwd <dir>] [--env NAME[=VALUE]]... [--verbose] -- <cmd...>
                               Copies the worktree (the git top level) into the VM, one way, skipping
                               .git, .env files and what git ignores. Runs the command there, in the
                               current directory or in <dir> relative to it. Absolute paths and ..
                               are refused; to reach a sibling directory, run from the top level.
                               Results such as build output stay in the VM and are not copied
                               back. --env works only from the host while the VM is online, to
                               prime build caches. --verbose prints which directory is copied where.`

func printVMExecUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:\n"+vmExecUsage)
}

var vmNoArgOps = map[string]vmOp{
	"down":  func(ctx context.Context, m vmManager, _ *Env) error { return m.Down(ctx) },
	"reset": func(ctx context.Context, m vmManager, _ *Env) error { return m.Reset(ctx) },
	"status": func(ctx context.Context, m vmManager, env *Env) error {
		r, err := m.Status(ctx)
		if err != nil {
			return err
		}
		r.Write(env.Stdout)
		return nil
	},
	"online":  func(ctx context.Context, m vmManager, _ *Env) error { return m.SetMode(ctx, vm.ModeOnline) },
	"offline": func(ctx context.Context, m vmManager, _ *Env) error { return m.SetMode(ctx, vm.ModeOffline) },
}

// parseVMImage validates `omac vm image …`; a nil op comes with the exit code.
func parseVMImage(args []string, env *Env) (vmOp, int) {
	if len(args) == 0 {
		return nil, vmMisuse(env, "image: missing verb (add|list|rm)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add", "rm":
		if len(rest) != 1 {
			return nil, vmMisuse(env, fmt.Sprintf("image %s: expected exactly one image reference", sub))
		}
		ref := rest[0]
		if _, err := vm.ValidateImageRef(ref); err != nil {
			return nil, vmMisuse(env, fmt.Sprintf("image %s: %v", sub, err))
		}
		if sub == "add" {
			return func(ctx context.Context, m vmManager, _ *Env) error { return m.ImageAdd(ctx, ref) }, ExitOK
		}
		return func(ctx context.Context, m vmManager, _ *Env) error { return m.ImageRemove(ctx, ref) }, ExitOK
	case "list":
		if len(rest) > 0 {
			return nil, vmMisuse(env, "image list takes no arguments")
		}
		return func(ctx context.Context, m vmManager, env *Env) error {
			refs, err := m.ImageList(ctx)
			if err != nil {
				return err
			}
			for _, r := range refs {
				fmt.Fprintln(env.Stdout, r)
			}
			return nil
		}, ExitOK
	default:
		return nil, vmMisuse(env, fmt.Sprintf("image: unknown verb %q", sub))
	}
}

func vmMisuse(env *Env, msg string) int {
	fmt.Fprintln(env.Stderr, "omac vm:", msg)
	printVMUsage(env)
	return ExitMisuse
}

// vmExitCode maps manager errors to stable exit codes.
func vmExitCode(err error) int {
	switch {
	case errors.Is(err, vm.ErrLimaMissing):
		return ExitPrerequisiteMissing
	case errors.Is(err, vm.ErrNotCreated),
		errors.Is(err, vm.ErrNotRunning),
		errors.Is(err, vm.ErrOnline),
		errors.Is(err, vm.ErrSessionsAttached),
		errors.Is(err, vm.ErrFirewallMissing):
		return ExitMisuse
	default:
		return ExitGeneric
	}
}

func printVMUsage(env *Env) {
	fmt.Fprintln(env.Stderr, `omac vm — persistent Lima VM that runs Docker for omac sessions

Every verb except list takes --name <x> to act on another VM than the default
"omac". A name is a lowercase letter, then up to 15 lowercase letters, digits
or '-'. Outside a session, vm exec uses --name; inside one it uses the VM the
session is attached to.

Usage:
  omac vm list                 Show every VM with its state and SSH port.
  omac vm up [--reprovision]   Create or start the VM. A new VM ends offline; a VM switched online stays online.
                               --reprovision reinstalls the packages and JDKs from vm.yaml, then switches offline.
  omac vm down                 Stop the VM.
  omac vm reset                Delete the VM with all its host-side state and create it anew.
  omac vm status               Show whether the VM runs, its mode, SSH port, images, JDKs, packages,
                               copied worktrees, attached sessions and the resources from vm.yaml or
                               their defaults (applied on create or reset).
  omac vm online               Restart the VM with internet access; refused while sessions are attached.
  omac vm offline              Restart the VM without internet access, the mode sessions need.
  omac vm image add <ref>      Pull an image on the host and load it into the VM's Docker.
  omac vm image list           Print the VM's images, one reference per line.
  omac vm image rm <ref>       Remove an image from the VM's Docker.
`+vmExecUsage)
}
