package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
	"github.com/TNG/oh-my-agentic-coder/internal/vm"
)

// vmSession is a session attached to the omac VM (--vm). A nil *vmSession
// means no --vm, and every method is then a no-op.
type vmSession struct {
	info      vm.AttachInfo
	stateRoot string
	env       map[string]string
	markers   *vmMarkers
	stderr    io.Writer
}

// attachVM registers the session with the VM, checks that the VM can take
// it, and prepares the agent's files in <tmpDir>/vm. tmpDir must be the
// session's temp dir, removed when omac exits normally, because it holds a
// copy of the agent's SSH key. The VM is never booted or stopped here; it
// outlives every session.
func attachVM(ctx context.Context, env *Env, tmpDir string) (*vmSession, error) {
	info, markers, err := prepareVMAttach(ctx, env)
	if err != nil {
		return nil, err
	}
	vmEnv, err := vmSandboxEnv(info, tmpDir)
	if err != nil {
		markers.release()
		return nil, err
	}
	return &vmSession{info: info, stateRoot: markers.layout.Root, env: vmEnv, markers: markers, stderr: env.Stderr}, nil
}

// grant opens the VM's Docker and SSH ports for the sandbox and lets
// DOCKER_HOST and DOCKER_CONFIG through its env filter. It also blocks the
// VM state this session attached to by its exact path, because the sandbox
// may resolve the baseline entries for it against another XDG_STATE_HOME,
// and the profile's override_deny cannot lift a --deny-literal.
func (s *vmSession) grant(argv []string, plan sandboxPlan) []string {
	if s == nil {
		return argv
	}
	argv = injectSandboxFlag(argv, "--deny-literal", s.stateRoot)
	argv = injectOpenPort(argv, strconv.Itoa(s.info.DockerPort))
	argv = injectOpenPort(argv, strconv.Itoa(s.info.SSHPort))
	return injectSandboxEnvAllow(argv, []string{"DOCKER_HOST", "DOCKER_CONFIG"}, plan)
}

func (s *vmSession) sandboxEnv() map[string]string {
	if s == nil {
		return nil
	}
	return s.env
}

// readyHook wraps next (may be nil) into the ready hook for
// sandbox.ExecWithReadyPID, so the sandboxed command is registered too.
func (s *vmSession) readyHook(next func(pid int)) func(pid int) {
	if s == nil {
		return next
	}
	return func(pid int) {
		s.trackChild(pid)
		if next != nil {
			next(pid)
		}
	}
}

// trackChild registers the sandboxed command, which keeps using the VM if
// omac dies without cleaning up (SIGKILL, os.Exit): its marker keeps
// `omac vm online` refused until that process is gone too.
func (s *vmSession) trackChild(pid int) {
	if s == nil {
		return
	}
	if err := s.markers.add(pid); err != nil {
		fmt.Fprintln(s.stderr, "omac: --vm: cannot register the sandboxed command with the VM:", err)
	}
}

// release removes every marker. Call it only once the sandboxed command
// has exited; on an os.Exit path leave the markers to be reaped.
func (s *vmSession) release() {
	if s == nil {
		return
	}
	s.markers.release()
}

// vmMarkers are the session markers of one attach, one per process.
type vmMarkers struct {
	layout   vm.Layout
	mu       sync.Mutex
	released bool
	removes  []func()
}

// add registers pid. After release it does nothing, because the ready hook
// can run after the child has already exited.
func (m *vmMarkers) add(pid int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.released {
		return nil
	}
	remove, err := vm.RegisterSession(m.layout, pid)
	if err != nil {
		return err
	}
	m.removes = append(m.removes, remove)
	return nil
}

func (m *vmMarkers) release() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.released = true
	for _, remove := range m.removes {
		remove()
	}
	m.removes = nil
}

// prepareVMAttach registers this process as attached, then checks that the
// omac VM can take a session. Registering first means `omac vm online`, which
// re-checks the markers after marking itself online, either sees this session
// or makes the check below fail. Tests replace it.
var prepareVMAttach = func(ctx context.Context, env *Env) (vm.AttachInfo, *vmMarkers, error) {
	m, err := newVMManager(env)
	if err != nil {
		return vm.AttachInfo{}, nil, err
	}
	root, err := vm.DefaultRoot()
	if err != nil {
		return vm.AttachInfo{}, nil, err
	}
	markers := &vmMarkers{layout: vm.NewLayout(root)}
	if err := markers.add(os.Getpid()); err != nil {
		return vm.AttachInfo{}, nil, err
	}
	info, err := m.PrepareAttach(ctx)
	if err != nil {
		markers.release()
		return vm.AttachInfo{}, nil, err
	}
	return info, markers, nil
}

// vmAttachExitCode maps an attach failure to an exit code. A missing Lima
// and every VM state the user fixes with `omac vm up` or `omac vm offline`
// are missing prerequisites of the session.
func vmAttachExitCode(err error) int {
	switch {
	case errors.Is(err, vm.ErrLimaMissing),
		errors.Is(err, vm.ErrNotCreated),
		errors.Is(err, vm.ErrNotRunning),
		errors.Is(err, vm.ErrOnline),
		errors.Is(err, vm.ErrFirewallMissing):
		return ExitPrerequisiteMissing
	default:
		return ExitGeneric
	}
}

// vmEnvNames are the variables vmSandboxEnv sets.
var vmEnvNames = []string{"DOCKER_HOST", "DOCKER_CONFIG", "OMAC_VM_HOST", "OMAC_VM_SSH_PORT", "OMAC_VM_SSH_KEY", "OMAC_VM_USER"}

// vmSandboxEnv creates <tmpDir>/vm with a copy of the agent key (the
// original lives in host-only VM state) and an empty DOCKER_CONFIG, and
// returns the variables that point the agent at the VM.
func vmSandboxEnv(info vm.AttachInfo, tmpDir string) (map[string]string, error) {
	key, err := os.ReadFile(info.AgentKeyPath)
	if err != nil {
		return nil, fmt.Errorf("agent key: %w", err)
	}
	if err := writeVMFiles(tmpDir, key); err != nil {
		return nil, err
	}
	dir := filepath.Join(tmpDir, "vm")
	return map[string]string{
		"DOCKER_HOST":      info.DockerHost(),
		"DOCKER_CONFIG":    filepath.Join(dir, "docker"),
		"OMAC_VM_HOST":     "127.0.0.1",
		"OMAC_VM_SSH_PORT": strconv.Itoa(info.SSHPort),
		"OMAC_VM_SSH_KEY":  filepath.Join(dir, "agent_key"),
		"OMAC_VM_USER":     info.User,
	}, nil
}

// writeVMFiles creates vm/, vm/docker/ and vm/agent_key in tmpDir. Other
// sandboxes may write tmpDir (on macOS every sandbox may write
// /var/folders), so it creates instead of reusing, and resolves through
// os.Root so a symlink planted inside tmpDir cannot redirect the key.
func writeVMFiles(tmpDir string, key []byte) error {
	root, err := os.OpenRoot(tmpDir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, d := range []string{"vm", filepath.Join("vm", "docker")} {
		if err := root.Mkdir(d, 0o700); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(filepath.Join("vm", "agent_key"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("agent key copy: %w", err)
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return fmt.Errorf("agent key copy: %w", err)
	}
	return f.Close()
}

// vmProfileMisuse returns why plan cannot carry a --vm session, or "".
// Only omac's own sandbox can open the VM's ports, it must keep the VM
// state protected, and its environment.deny_vars must not strip the VM
// variables: FilterEnv applies deny_vars after every grant.
func vmProfileMisuse(plan sandboxPlan) string {
	if !plan.Native {
		return fmt.Sprintf("--vm needs omac's own sandbox; launcher profile %q runs a different sandbox, so omac cannot open the VM's ports there", plan.Name)
	}
	if launcherLearns(plan.Launcher.Command) {
		return fmt.Sprintf("--vm: launcher profile %q runs the sandbox with --learn, which would let the agent reach the VM's admin key", plan.Name)
	}
	if plan.Policy == nil {
		return fmt.Sprintf("--vm: sandbox profile %q could not be loaded (%v), so omac cannot check that it keeps the VM state protected", plan.PolicyRef, plan.PolicyErr)
	}
	root, err := vm.DefaultRoot()
	if err != nil {
		return "--vm: " + err.Error()
	}
	if _, ok := sandboxrun.NewProtectedPathSet(plan.Policy).IsProtected(root); !ok {
		return fmt.Sprintf("--vm: the sandbox could reach the VM state in %s; sandbox profile %q must keep it protected (check filesystem.override_deny)", root, plan.PolicyRef)
	}
	for _, name := range vmEnvNames {
		if sandboxprofile.EnvDenied(name, plan.Policy.Environment.DenyVars) {
			return fmt.Sprintf("--vm: sandbox profile %q denies %s in environment.deny_vars, so the agent would not see the VM; remove the matching entry", plan.PolicyRef, name)
		}
	}
	return ""
}

// launcherLearns reports whether a `{{self}} sandbox run` launcher command
// passes --learn, which lifts every filesystem restriction. A command the
// sandbox's own parser rejects, e.g. for a templated port, is scanned
// instead, and a --learn anywhere before "--" counts.
func launcherLearns(command []string) bool {
	if len(command) < 3 {
		return false
	}
	args := command[3:]
	if f, err := sandboxprofile.ParseFlags(args); err == nil {
		return f.Learn
	}
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--learn" {
			return true
		}
	}
	return false
}
