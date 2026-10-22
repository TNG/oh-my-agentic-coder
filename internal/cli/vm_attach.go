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

// prepareVMAttach checks that the omac VM can take a session and registers
// this process as attached. The returned func removes the registration; it
// never stops the VM, which outlives every session. Tests replace it.
var prepareVMAttach = func(ctx context.Context, env *Env) (vm.AttachInfo, func(), error) {
	m, err := newVMManager(env)
	if err != nil {
		return vm.AttachInfo{}, nil, err
	}
	info, err := m.PrepareAttach(ctx)
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
		if err := os.MkdirAll(d, 0o700); err != nil {
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

// injectVMGrants opens the VM's Docker and SSH ports for the sandbox and
// lets DOCKER_HOST and DOCKER_CONFIG through its env filter. Only omac's
// native backend understands these flags; other backends get a warning.
func injectVMGrants(env *Env, argv []string, info vm.AttachInfo, plan sandboxPlan) []string {
	if !plan.Native {
		fmt.Fprintln(env.Stderr, "omac: --vm applies only to the native sandbox backend; its ports and DOCKER_HOST are not granted on this profile.")
		return argv
	}
	argv = injectOpenPort(argv, strconv.Itoa(info.DockerPort))
	argv = injectOpenPort(argv, strconv.Itoa(info.SSHPort))
	return injectSandboxEnvAllow(argv, []string{"DOCKER_HOST", "DOCKER_CONFIG"}, plan)
}
