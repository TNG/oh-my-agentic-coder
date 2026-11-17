package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/TNG/oh-my-agentic-coder/internal/prefs"
)

// InstanceName is the Lima instance name of every VM; each VM has its own
// LIMA_HOME.
const InstanceName = "omac"

// Layout names every host-side file of the VM state directory.
type Layout struct {
	Root        string // the state directory itself
	LimaHome    string // LIMA_HOME; short, because <LimaHome>/omac/ssh.sock.* must stay under the 104-byte socket-path limit
	AdminKey    string // root access, host only
	AgentKey    string // agent user, copied into the session temp dir at attach
	StatePath   string // state.json
	KnownHosts  string // ssh known_hosts for 127.0.0.1:<port>
	SessionsDir string // one marker file per attached process, named by pid
	LimaYAML    string // the rendered template handed to `limactl create`
}

// DefaultVMName is the VM omac manages when no --name is given.
const DefaultVMName = "omac"

var vmNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)

// ValidateName accepts the names a VM may have: a lowercase letter, then up
// to 15 lowercase letters, digits or hyphens. "true" and "false" are
// reserved, because `--vm=true` and `--vm=false` switch the flag on and off.
func ValidateName(name string) error {
	if name == "true" || name == "false" {
		return fmt.Errorf("invalid VM name %q: true and false are reserved", name)
	}
	if !vmNameRE.MatchString(name) {
		return fmt.Errorf("invalid VM name %q: use a lowercase letter, then up to 15 lowercase letters, digits or '-'", name)
	}
	return nil
}

// stateHome returns the XDG_STATE_HOME the VM state lives under.
var stateHome = func() string { return os.Getenv("XDG_STATE_HOME") }

// PinStateHome makes the VM state resolve against dir ("" means
// $HOME/.local/state) instead of the current XDG_STATE_HOME, until the
// returned func restores it. `omac serve --for-opencode-desktop` needs this:
// it points XDG_STATE_HOME at the OpenCode Desktop app's data folder.
func PinStateHome(dir string) (restore func()) {
	prev := stateHome
	stateHome = func() string { return dir }
	return func() { stateHome = prev }
}

// stateBase is <state home>/omac.
func stateBase() (string, error) {
	home, err := stateHomeDir(stateHome())
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "omac"), nil
}

// stateHomeDir applies the XDG Base Directory rule to an XDG_STATE_HOME
// value: an absolute value is the state home; an empty or relative one is
// ignored in favour of $HOME/.local/state, on macOS too.
func stateHomeDir(xdg string) (string, error) {
	if filepath.IsAbs(xdg) {
		return xdg, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("omac vm: cannot resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state"), nil
}

// InstanceRoot resolves the state directory of the named VM: the default VM
// keeps <state>/vm, every other one gets <state>/vms/<name>.
func InstanceRoot(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", fmt.Errorf("omac vm: %w", err)
	}
	base, err := stateBase()
	if err != nil {
		return "", err
	}
	if name == DefaultVMName {
		return filepath.Join(base, "vm"), nil
	}
	return filepath.Join(base, "vms", name), nil
}

// deleteVMHint tells how to delete a VM by hand, with the real state paths.
func deleteVMHint() string {
	base, err := stateBase()
	if err != nil {
		return "delete a VM you no longer need"
	}
	dir := filepath.Join(base, "vms", "<name>")
	return fmt.Sprintf("to delete a VM you no longer need (see `omac vm list`), run `LIMA_HOME=%s limactl delete -f %s`, then remove %s (for the VM omac: %s)",
		filepath.Join(dir, "lima"), InstanceName, dir, filepath.Join(base, "vm"))
}

// lockPortPick holds an exclusive lock, shared by every omac process, until
// unlock is called, so two new VMs never pick the same ports.
func lockPortPick() (unlock func(), err error) {
	base, err := stateBase()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "vms")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("omac vm: lock port pick: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, ".ports.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("omac vm: lock port pick: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("omac vm: lock port pick: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// NewLayout derives all paths from root.
func NewLayout(root string) Layout {
	return Layout{
		Root:        root,
		LimaHome:    filepath.Join(root, "lima"),
		AdminKey:    filepath.Join(root, "admin_key"),
		AgentKey:    filepath.Join(root, "agent_key"),
		StatePath:   filepath.Join(root, "state.json"),
		KnownHosts:  filepath.Join(root, "known_hosts"),
		SessionsDir: filepath.Join(root, "sessions"),
		LimaYAML:    filepath.Join(root, "omac.yaml"),
	}
}

// InstanceConfigPath is the config file of the named VM: vm.yaml for the
// default VM, vms/<name>.yaml next to it for the others. "" when no global
// config directory resolves.
func InstanceConfigPath(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", fmt.Errorf("omac vm: %w", err)
	}
	dir := prefs.GlobalDir()
	if dir == "" {
		return "", nil
	}
	if name == DefaultVMName {
		return filepath.Join(dir, "vm.yaml"), nil
	}
	return filepath.Join(dir, "vms", name+".yaml"), nil
}

// InstanceRow is one line of `omac vm list`.
type InstanceRow struct {
	Name      string
	Mode      string // "not created" when state.json is missing
	SSHPort   int    // 0 when state.json is missing
	PortBlock int
	Err       error // set, with Mode "error", when state.json cannot be read
}

// ListInstances reads the state of the default VM and of every VM under
// <state>/vms, sorted by name with the default VM first. Directories that
// are not valid VM names are skipped.
func ListInstances() ([]InstanceRow, error) {
	base, err := stateBase()
	if err != nil {
		return nil, err
	}
	names := []string{}
	if isDir(filepath.Join(base, "vm")) {
		names = append(names, DefaultVMName)
	}
	entries, err := os.ReadDir(filepath.Join(base, "vms"))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("omac vm: list VMs: %w", err)
	}
	for _, e := range entries { // ReadDir sorts by name
		if e.IsDir() && e.Name() != DefaultVMName && ValidateName(e.Name()) == nil {
			names = append(names, e.Name())
		}
	}
	rows := make([]InstanceRow, 0, len(names))
	for _, n := range names {
		root, err := InstanceRoot(n)
		if err != nil {
			return nil, err
		}
		st, err := LoadState(NewLayout(root).StatePath)
		row := InstanceRow{Name: n, Mode: "not created"}
		if err != nil {
			row.Mode, row.Err = "error", err
		} else if st != nil {
			row.Mode, row.SSHPort, row.PortBlock = st.Mode, st.SSHPort, st.PortBlock
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
