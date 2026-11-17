package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

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
var newVMManager = func(env *Env) (vmManager, error) {
	root, err := vm.DefaultRoot()
	if err != nil {
		return nil, err
	}
	return vm.NewManager(vm.NewLayout(root), vm.UserConfigPath(), vm.RealDeps(), env.Stdout), nil
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
	var op vmOp
	switch verb {
	case "up":
		reprovision := false
		for _, a := range rest {
			if a != "--reprovision" {
				return vmMisuse(env, fmt.Sprintf("up: unexpected argument %q", a))
			}
			reprovision = true
		}
		op = func(ctx context.Context, m vmManager, _ *Env) error { return m.Up(ctx, reprovision) }
	case "down", "reset", "status", "online", "offline":
		if len(rest) > 0 {
			return vmMisuse(env, fmt.Sprintf("%s takes no arguments", verb))
		}
		op = vmNoArgOp(verb)
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
		return vmMisuse(env, fmt.Sprintf("unknown verb %q", verb))
	}

	// Errors print as returned: most Manager errors already start with
	// "omac vm", and a prefix here would repeat it.
	m, err := newVMManager(env)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return ExitIOError
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := op(ctx, m, env); err != nil {
		fmt.Fprintln(env.Stderr, err)
		if errors.Is(err, vm.ErrLimaMissing) {
			fmt.Fprintln(env.Stderr, "omac vm: install Lima first, e.g. `brew install lima`")
		}
		return vmExitCode(err)
	}
	return ExitOK
}

// resolveVMConn finds the VM's ssh connection: from the OMAC_VM_*
// variables inside a sandbox, else from the host-side state directory.
var resolveVMConn = func(*Env) (vm.ConnInfo, error) {
	root, err := vm.DefaultRoot()
	if err != nil {
		return vm.ConnInfo{}, err
	}
	return vm.ResolveConn(os.Getenv, vm.NewLayout(root), vm.RealDeps().HostUser)
}

// isTerminal reports whether f is a terminal. Tests replace it.
var isTerminal = func(f *os.File) bool { return f != nil && term.IsTerminal(int(f.Fd())) }

// loadVMState reads the host-side state.json; nil when the VM was never
// created. Tests replace it.
var loadVMState = func() (*vm.State, error) {
	root, err := vm.DefaultRoot()
	if err != nil {
		return nil, err
	}
	return vm.LoadState(vm.NewLayout(root).StatePath)
}

// runVMExecFn is vm.Exec; tests replace it so no VM is touched.
var runVMExecFn = vm.Exec

// vmExecArgs is a parsed `omac vm exec` invocation.
type vmExecArgs struct {
	cwd     string
	envs    []string
	cmd     []string
	verbose bool
}

// parseVMExecArgs parses [--cwd <dir>] [--env SPEC]... [--verbose] [--] <cmd...>.
// Flags end at "--" or at the first argument that is not a flag, so the
// command keeps its own flags. ok is false when the caller should exit with
// code: after -h/--help or a usage error.
func parseVMExecArgs(args []string, env *Env) (a vmExecArgs, code int, ok bool) {
	fs := flag.NewFlagSet("omac vm exec", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
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
	if err := vm.ValidateCwd(a.cwd); err != nil {
		return a, vmExecMisuse(env, strings.TrimPrefix(err.Error(), "omac vm exec: ")), false
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
	envs, err := vm.ResolveEnv(a.envs, os.Getenv)
	if err != nil {
		return vmExecMisuse(env, strings.TrimPrefix(err.Error(), "omac vm exec: "))
	}
	conn, err := resolveVMConn(env)
	if err != nil {
		fmt.Fprintln(env.Stderr, err) // ErrNotCreated already says to run `omac vm up`
		if errors.Is(err, vm.ErrNotCreated) {
			return ExitPrerequisiteMissing
		}
		return ExitIOError
	}
	if len(envs) > 0 {
		if code, ok := checkVMExecEnvAllowed(env, conn); !ok {
			return code
		}
	}
	var logf func(string, ...any)
	if a.verbose {
		logf = func(format string, v ...any) { fmt.Fprintf(env.Stderr, format+"\n", v...) }
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err = runVMExecFn(ctx, vm.ExecOpts{
		Workdir: env.Workdir,
		Cwd:     a.cwd,
		Env:     envs,
		Cmd:     a.cmd,
		Conn:    conn,
		Run:     vm.ExecRunner,
		SSH:     vm.DefaultSSH,
		// A terminal in the VM turns "\n" into "\r\n" and mixes stderr into
		// stdout, which would corrupt `omac vm exec -- cat x > x`.
		TTY:    isTerminal(env.Stdin) && isTerminal(env.Stdout),
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

// checkVMExecEnvAllowed limits --env to priming: from the host, while the
// VM is online, which also means no session is attached.
func checkVMExecEnvAllowed(env *Env, conn vm.ConnInfo) (int, bool) {
	const reason = "--env is for priming from the host while the VM is online"
	if conn.FromSandbox {
		return vmExecMisuse(env, reason), false
	}
	st, err := loadVMState()
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return ExitIOError, false
	}
	if st == nil || st.Mode != vm.ModeOnline {
		return vmExecMisuse(env, reason+"; run `omac vm online` first"), false
	}
	return ExitOK, true
}

func vmExecMisuse(env *Env, msg string) int {
	fmt.Fprintln(env.Stderr, "omac vm exec:", msg)
	printVMExecUsage(env.Stderr)
	return ExitMisuse
}

const vmExecUsage = `  omac vm exec [--cwd <dir>] [--env NAME[=VALUE]]... [--verbose] -- <cmd...>
                               Copies the worktree (the git top level) into the VM, one way, skipping
                               .git, .env files and what git ignores. Runs the command there, in the
                               current directory or in <dir> relative to it. Results such as build
                               output stay in the VM and are not copied back.`

func printVMExecUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:\n"+vmExecUsage)
}

func vmNoArgOp(verb string) vmOp {
	switch verb {
	case "down":
		return func(ctx context.Context, m vmManager, _ *Env) error { return m.Down(ctx) }
	case "reset":
		return func(ctx context.Context, m vmManager, _ *Env) error { return m.Reset(ctx) }
	case "status":
		return func(ctx context.Context, m vmManager, env *Env) error {
			r, err := m.Status(ctx)
			if err != nil {
				return err
			}
			r.Write(env.Stdout)
			return nil
		}
	default: // online, offline
		return func(ctx context.Context, m vmManager, _ *Env) error { return m.SetMode(ctx, verb) }
	}
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

Usage:
  omac vm up [--reprovision]   Create or start the VM offline; --reprovision reinstalls the packages and JDKs from vm.yaml.
  omac vm down                 Stop the VM.
  omac vm reset                Delete the VM with all its host-side state and create it anew.
  omac vm status               Show whether the VM runs, its mode, ports, resources, images, JDKs, packages, copied worktrees and attached sessions.
  omac vm online               Restart the VM with internet access; refused while sessions are attached.
  omac vm offline              Restart the VM without internet access, the mode sessions need.
  omac vm image add <ref>      Pull an image on the host and load it into the VM's Docker.
  omac vm image list           Print the VM's images, one reference per line.
  omac vm image rm <ref>       Remove an image from the VM's Docker.
`+vmExecUsage)
}
