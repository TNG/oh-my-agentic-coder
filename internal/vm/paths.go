package vm

import (
	"fmt"
	"os"
	"path/filepath"

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

// DefaultRoot resolves the state directory: $XDG_STATE_HOME/omac/vm, else
// $HOME/.local/state/omac/vm on every platform (the spec wants the same
// path on macOS).
func DefaultRoot() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "omac", "vm"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("omac vm: cannot resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "omac", "vm"), nil
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
func UserConfigPath() string {
	dir := prefs.GlobalDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "vm.yaml")
}
