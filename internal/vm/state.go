package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// VM modes. Sessions may attach only in ModeOffline.
const (
	ModeOffline = "offline"
	ModeOnline  = "online"
)

// StateVersion is the current state.json schema version.
const StateVersion = 1

// State is state.json: what omac last set up in the VM.
type State struct {
	Version    int      `json:"version"`
	SSHPort    int      `json:"ssh_port"`
	DockerPort int      `json:"docker_port"`
	Mode       string   `json:"mode"`
	Images     []string `json:"images"`
	Worktrees  []string `json:"worktrees"`
	JDKs       []string `json:"jdks"`
	Packages   []string `json:"packages"` // extra packages from the last provisioning
}

// LoadState reads state.json. A missing file yields nil, nil.
func LoadState(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("omac vm: read state: %w", err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("omac vm: parse %s: %w", path, err)
	}
	if st.Version == 0 {
		st.Version = StateVersion
	}
	return &st, nil
}

// SaveState writes state.json atomically (temp file in the same directory,
// then rename) with mode 0600, creating the directory with 0700.
func SaveState(path string, st *State) error {
	if st.Version == 0 {
		st.Version = StateVersion
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("omac vm: marshal state: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("omac vm: state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "state.json.tmp-*")
	if err != nil {
		return fmt.Errorf("omac vm: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("omac vm: write state: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("omac vm: chmod state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("omac vm: close state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("omac vm: rename state: %w", err)
	}
	return nil
}
