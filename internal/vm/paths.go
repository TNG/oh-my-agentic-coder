package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/TNG/oh-my-agentic-coder/internal/prefs"
)

// InstanceName is the single Lima instance omac manages.
const InstanceName = "omac"

// Layout names every host-side file of the VM state directory.
type Layout struct {
	Root        string // the state directory itself
	LimaHome    string // LIMA_HOME; short, because <LimaHome>/omac/ssh.sock.* must stay under the 104-byte socket-path limit
	AdminKey    string // root access, host only
	AgentKey    string // agent user, copied into the session cache scope at attach
	StatePath   string // state.json
	KnownHosts  string // ssh known_hosts for 127.0.0.1:<port>
	SessionsDir string // one marker file per attached session, named by pid
	LimaYAML    string // the rendered template handed to `limactl create`
}

// DefaultVMName is the VM omac manages when no --name is given.
const DefaultVMName = "omac"

var vmNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)

// ValidateName accepts the names a VM may have: a lowercase letter, then up
// to 15 lowercase letters, digits or hyphens.
func ValidateName(name string) error {
	if !vmNameRE.MatchString(name) {
		return fmt.Errorf("invalid VM name %q: use a lowercase letter, then up to 15 lowercase letters, digits or '-'", name)
	}
	return nil
}

// stateBase is $XDG_STATE_HOME/omac, else $HOME/.local/state/omac on every
// platform (the spec wants the same path on macOS).
func stateBase() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "omac"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("omac vm: cannot resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "omac"), nil
}

// DefaultRoot resolves the state directory of the default VM.
func DefaultRoot() (string, error) { return InstanceRoot(DefaultVMName) }

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

// UserConfigPath is ~/.config/omac/vm.yaml (honoring $XDG_CONFIG_HOME), or
// "" when no global config directory resolves.
func UserConfigPath() string { return InstanceConfigPath(DefaultVMName) }

// InstanceConfigPath is the config file of the named VM: vm.yaml for the
// default VM, vms/<name>.yaml next to it for the others. "" when no global
// config directory resolves.
func InstanceConfigPath(name string) string {
	dir := prefs.GlobalDir()
	if dir == "" {
		return ""
	}
	if name == DefaultVMName {
		return filepath.Join(dir, "vm.yaml")
	}
	return filepath.Join(dir, "vms", name+".yaml")
}

// InstanceRow is one line of `omac vm list`.
type InstanceRow struct {
	Name      string
	Mode      string // "not created" when state.json is missing
	SSHPort   int    // 0 when state.json is missing
	PortBlock int
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
		if err != nil {
			return nil, err
		}
		row := InstanceRow{Name: n, Mode: "not created"}
		if st != nil {
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
