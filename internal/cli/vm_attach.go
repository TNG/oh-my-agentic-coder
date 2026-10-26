package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/TNG/oh-my-agentic-coder/internal/vm"
)

// prepareVMAttach registers this process as attached, then checks that the
// omac VM can take a session. Registering first means `omac vm online`, which
// re-checks the markers after marking itself online, either sees this session
// or makes the check below fail. The returned func removes the registration;
// it never stops the VM, which outlives every session. Tests replace it.
var prepareVMAttach = func(ctx context.Context, env *Env) (vm.AttachInfo, func(), error) {
	m, err := newVMManager(env)
	if err != nil {
		return vm.AttachInfo{}, nil, err
	}
	root, err := vm.DefaultRoot()
	if err != nil {
		return vm.AttachInfo{}, nil, err
	}
	release, err := vm.RegisterSession(vm.NewLayout(root), os.Getpid())
	if err != nil {
		return vm.AttachInfo{}, nil, err
	}
	info, err := m.PrepareAttach(ctx)
	if err != nil {
		release()
		return vm.AttachInfo{}, nil, err
	}
	return info, release, nil
}

// vmAttachExitCode maps an attach failure to an exit code. Every VM state
// the user fixes with `omac vm up` or `omac vm offline` is a missing
// prerequisite of the session.
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

// vmSandboxEnv prepares <scopeDir>/vm, a directory the sandbox may write,
// and returns the variables that point the agent at the VM. The agent key is
// copied there because the original lives in host-only VM state.
func vmSandboxEnv(info vm.AttachInfo, scopeDir string) (map[string]string, error) {
	dir := filepath.Join(scopeDir, "vm")
	dockerDir := filepath.Join(dir, "docker")
	for _, d := range []string{dir, dockerDir} {
		// The sandbox can write scopeDir; refuse a planted symlink or file
		// instead of following it with the key copy or chmod.
		if fi, err := os.Lstat(d); err == nil && !fi.IsDir() {
			return nil, fmt.Errorf("%s exists but is not a directory", d)
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err := os.Mkdir(d, 0o700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, err
		}
	}
	key, err := os.ReadFile(info.AgentKeyPath)
	if err != nil {
		return nil, fmt.Errorf("agent key: %w", err)
	}
	keyPath := filepath.Join(dir, "agent_key")
	if err := writeFileAtomic(keyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("agent key copy: %w", err)
	}
	return map[string]string{
		"DOCKER_HOST":      info.DockerHost(),
		"DOCKER_CONFIG":    dockerDir,
		"OMAC_VM_HOST":     "127.0.0.1",
		"OMAC_VM_SSH_PORT": strconv.Itoa(info.SSHPort),
		"OMAC_VM_SSH_KEY":  keyPath,
		"OMAC_VM_USER":     info.User,
	}, nil
}

// writeFileAtomic replaces path via rename, so a concurrent session sharing
// the cache directory never reads a half-written key.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// vmProfileMisuse is the usage error for --vm on a launcher profile that
// runs a sandbox other than omac's own: only that backend can open the
// VM's ports. A custom policy file for omac's sandbox is fine.
func vmProfileMisuse(plan sandboxPlan) string {
	return fmt.Sprintf("--vm needs omac's own sandbox; launcher profile %q runs a different sandbox, so omac cannot open the VM's ports there", plan.Name)
}

// injectVMGrants opens the VM's Docker and SSH ports for the sandbox and
// lets DOCKER_HOST and DOCKER_CONFIG through its env filter. Callers refuse
// --vm on non-native plans first (vmProfileMisuse).
func injectVMGrants(argv []string, info vm.AttachInfo, plan sandboxPlan) []string {
	argv = injectOpenPort(argv, strconv.Itoa(info.DockerPort))
	argv = injectOpenPort(argv, strconv.Itoa(info.SSHPort))
	return injectSandboxEnvAllow(argv, []string{"DOCKER_HOST", "DOCKER_CONFIG"}, plan)
}
