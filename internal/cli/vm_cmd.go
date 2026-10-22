package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

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

// runVMExecFn is vm.Exec; tests replace it so no VM is touched.
var runVMExecFn = vm.Exec

// vmExecArgs is a parsed `omac vm exec` invocation.
type vmExecArgs struct {
	cwd     string
	envs    []string
	cmd     []string
	verbose bool
}

// parseVMExecArgs parses [--cwd <dir>] [--env SPEC]... [--verbose] -- <cmd...>.
// Without "--", the first non-flag argument starts the command. help is
// true for -h/--help.
func parseVMExecArgs(args []string) (a vmExecArgs, help bool, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			a.cmd = args[i+1:]
			break
		}
		if !strings.HasPrefix(arg, "-") {
			a.cmd = args[i:]
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "-h", "--help":
			return a, true, nil
		case "--verbose":
			if hasValue {
				return a, false, errors.New("--verbose takes no value")
			}
			a.verbose = true
			continue
		case "--cwd", "--env":
		default:
			return a, false, fmt.Errorf("unknown flag %q", arg)
		}
		if !hasValue {
			if i+1 >= len(args) {
				return a, false, fmt.Errorf("%s needs a value", name)
			}
			i++
			value = args[i]
		}
		if name == "--cwd" {
			a.cwd = value
		} else {
			a.envs = append(a.envs, value)
		}
	}
	if len(a.cmd) == 0 {
		return a, false, errors.New("no command given")
	}
	return a, false, nil
}

// runVMExec runs `omac vm exec`: copy the worktree into the VM, run the
// command there, return its exit code.
func runVMExec(args []string, env *Env) int {
	a, help, err := parseVMExecArgs(args)
	if help {
		printVMExecUsage(env.Stdout)
		return ExitOK
	}
	if err != nil {
		return vmExecMisuse(env, err.Error())
	}
	envs, err := vm.ResolveEnv(a.envs, os.Getenv)
	if err != nil {
		return vmExecMisuse(env, strings.TrimPrefix(err.Error(), "omac vm exec: "))
	}
	conn, err := resolveVMConn(env)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		if errors.Is(err, vm.ErrNotCreated) {
			fmt.Fprintln(env.Stderr, "omac vm exec: run `omac vm up` first")
			return ExitPrerequisiteMissing
		}
		return ExitIOError
	}
	var logf func(string, ...any)
	if a.verbose {
		logf = func(format string, v ...any) { fmt.Fprintf(env.Stderr, format+"\n", v...) }
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := runVMExecFn(ctx, vm.ExecOpts{
		Workdir: env.Workdir,
		Cwd:     a.cwd,
		Env:     envs,
		Cmd:     a.cmd,
		Conn:    conn,
		Run:     vm.ExecRunner,
		SSH:     vm.DefaultSSH,
		Stdin:   env.Stdin,
		Stdout:  env.Stdout,
		Stderr:  env.Stderr,
		Log:     logf,
	})
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return ExitIOError
	}
	return code
}

func vmExecMisuse(env *Env, msg string) int {
	fmt.Fprintln(env.Stderr, "omac vm exec:", msg)
	printVMExecUsage(env.Stderr)
	return ExitMisuse
}

const vmExecUsage = `  omac vm exec [--cwd <dir>] [--env NAME[=VALUE]]... [--verbose] -- <cmd...>
                               Copies the worktree into the VM (one way, skipping what git ignores),
                               runs the command there, in <dir> relative to the worktree if given;
                               results such as build output stay in the VM and are not copied back.`

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
  omac vm up [--reprovision]   Create or start the VM offline; --reprovision reinstalls the packages from vm.yaml.
  omac vm down                 Stop the VM.
  omac vm reset                Delete the VM with all its host-side state and create it anew.
  omac vm status               Show whether the VM runs, its mode, ports, resources, images and sessions.
  omac vm online               Restart the VM with internet access; refused while sessions are attached.
  omac vm offline              Restart the VM without internet access, the mode sessions need.
  omac vm image add <ref>      Pull an image on the host and load it into the VM's Docker.
  omac vm image list           Print the VM's images, one reference per line.
  omac vm image rm <ref>       Remove an image from the VM's Docker.
`+vmExecUsage)
}
