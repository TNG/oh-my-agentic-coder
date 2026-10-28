package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
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
	// "exec" is reserved for the next PR (run a command against the VM's Docker).
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
  omac vm image rm <ref>       Remove an image from the VM's Docker.`)
}
