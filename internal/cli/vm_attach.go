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

// vmFlag is the value of --vm: bare `--vm` attaches to the VM the project map
// assigns, `--vm=<name>` to that VM. `--vm <name>` with a space is not
// supported, because a bool-style flag cannot take the next argument.
type vmFlag struct {
	on   bool
	name string
}

func (f *vmFlag) IsBoolFlag() bool { return true }

func (f *vmFlag) String() string {
	switch {
	case f == nil || !f.on:
		return "false"
	case f.name == "":
		return "true"
	}
	return f.name
}

func (f *vmFlag) Set(s string) error {
	switch s {
	case "true":
		*f = vmFlag{on: true}
	case "false":
		*f = vmFlag{}
	default:
		if err := vm.ValidateName(s); err != nil {
			return err
		}
		*f = vmFlag{on: true, name: s}
	}
	return nil
}

const vmFlagUsage = "Attach to an omac VM, --vm[=<name>]: its SSH port is opened for the sandbox, so `omac vm exec` runs Docker, Gradle and tests there. Without a name, the VM that vm-projects.yaml assigns to the project, else `omac`. Requires `omac vm up` and offline mode."

// vmSelection is the VM a session attaches to and why.
type vmSelection struct {
	name   string
	source string // for messages: where the name came from
}

// selectVM picks the VM for a session in dir: the explicit --vm=<name>, else
// the project map entry of dir's repository, else the default VM.
func selectVM(explicit, dir string) (vmSelection, error) {
	if explicit != "" {
		return vmSelection{name: explicit, source: "--vm=" + explicit}, nil
	}
	name, ok, err := vm.MappedInstance(dir)
	if err != nil {
		return vmSelection{}, err
	}
	if ok {
		return vmSelection{name: name, source: vm.ProjectMapPath()}, nil
	}
	return vmSelection{name: vm.DefaultVMName}, nil
}

// errNoVMInstance is the abort for a chosen VM that was never created. It
// matches vm.ErrNotCreated, so the exit code says a prerequisite is missing.
type errNoVMInstance struct{ vmSelection }

func (e errNoVMInstance) Error() string {
	return fmt.Sprintf("VM %q (from %s) does not exist; run `omac vm up --name %s`", e.name, e.source, e.name)
}

func (e errNoVMInstance) Is(target error) bool { return target == vm.ErrNotCreated }

// prepareVMAttach selects the VM for a session in dir, registers this process
// as attached, then checks that the VM can take a session. Registering first
// means `omac vm online`, which re-checks the markers after marking itself
// online, either sees this session or makes the check below fail. The
// returned func removes the registration; it never stops the VM, which
// outlives every session. Tests replace it.
var prepareVMAttach = func(ctx context.Context, env *Env, explicit, dir string) (vm.AttachInfo, func(), error) {
	sel, err := selectVM(explicit, dir)
	if err != nil {
		return vm.AttachInfo{}, nil, err
	}
	if sel.name != vm.DefaultVMName {
		st, err := loadVMState(sel.name)
		if err != nil {
			return vm.AttachInfo{}, nil, err
		}
		if st == nil {
			return vm.AttachInfo{}, nil, errNoVMInstance{sel}
		}
	}
	m, err := newVMManager(env, sel.name)
	if err != nil {
		return vm.AttachInfo{}, nil, err
	}
	root, err := vm.InstanceRoot(sel.name)
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
		return vm.AttachInfo{}, nil, vm.WithNameHints(sel.name, err)
	}
	info.Name = sel.name
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

// vmSandboxEnv prepares <scopeDir>/vm (<scopeDir>/vm/<name> for a VM other
// than the default), a directory the sandbox may write, and returns the
// variables that point the agent at the VM. The agent key is copied there
// because the original lives in host-only VM state. Sessions on different
// VMs can share scopeDir, so each VM needs its own directory.
func vmSandboxEnv(info vm.AttachInfo, scopeDir string) (map[string]string, error) {
	dirs := []string{filepath.Join(scopeDir, "vm")}
	if info.Name != "" && info.Name != vm.DefaultVMName {
		dirs = append(dirs, filepath.Join(dirs[0], info.Name))
	}
	dir := dirs[len(dirs)-1]
	for _, d := range dirs {
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
	if err := writeVMFileAtomic(keyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("agent key copy: %w", err)
	}
	// ssh in the sandbox reads known_hosts next to the key copy (see
	// vm.ConnFromEnv); with the host's pinned entry it verifies the VM
	// instead of trusting the first key it sees.
	hosts, err := os.ReadFile(filepath.Join(filepath.Dir(info.AgentKeyPath), "known_hosts"))
	switch {
	case err == nil:
		if err := writeVMFileAtomic(filepath.Join(dir, "known_hosts"), hosts, 0o600); err != nil {
			return nil, fmt.Errorf("known_hosts copy: %w", err)
		}
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	first, last := vm.PortBlockRange(info.PortBlock)
	return map[string]string{
		vm.EnvVMHost:      "127.0.0.1",
		vm.EnvVMSSHPort:   strconv.Itoa(info.SSHPort),
		vm.EnvVMSSHKey:    keyPath,
		vm.EnvVMUser:      info.User,
		vm.EnvVMName:      info.Name,
		vm.EnvVMPortFirst: strconv.Itoa(first),
		vm.EnvVMPortLast:  strconv.Itoa(last),
	}, nil
}

// writeVMFileAtomic replaces path via rename, so a concurrent session sharing
// the cache directory never reads a half-written key.
func writeVMFileAtomic(path string, data []byte, perm os.FileMode) error {
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
// VM's SSH port. A custom policy file for omac's sandbox is fine.
func vmProfileMisuse(plan sandboxPlan) string {
	return fmt.Sprintf("--vm needs omac's own sandbox; launcher profile %q runs a different sandbox, so omac cannot open the VM's SSH port there", plan.Name)
}

// injectVMGrants opens the VM's SSH port for the sandbox. Callers refuse
// --vm on non-native plans first (vmProfileMisuse).
func injectVMGrants(argv []string, info vm.AttachInfo) []string {
	return injectOpenPort(argv, strconv.Itoa(info.SSHPort))
}
