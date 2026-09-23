package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Project sandbox configuration lives in the agent-writable workdir. On Linux
// the sandbox masks it read-only, but the macOS Seatbelt backend cannot block a
// directory-entry replacement: an agent can delete <workdir>/.omac and move a
// prepared directory into its place, and the next launch would read the
// attacker's config. Trust therefore has to be anchored host-side: the content
// a project contributes to a launch is pinned under ~/.config/omac (host-only,
// invisible to the sandbox) the first time it is used, and a later launch
// aborts the launch when the content no longer matches.

// projectPinsFile is the host-only store of approved project sandbox content.
type projectPinsFile struct {
	Projects map[string]projectPin `json:"projects"`
}

// projectPin is the approved content hash for one workdir.
type projectPin struct {
	// Hash covers the local launcher config and, when the local layer selects
	// the profile, the selected profile file. Absent files hash as "absent".
	Hash string `json:"hash"`
}

func projectPinsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "omac", "project-sandbox.json")
}

// ProjectSandboxContentHash digests the project-local artifacts that can steer
// a launch: the .omac/config.yaml (or its absence) and, when sel came from the
// workdir layer, the selected profile file. An empty hash means there is no
// local contribution to pin.
func ProjectSandboxContentHash(workdir string, sel ProfileSelection) (string, error) {
	if workdir == "" {
		return "", nil
	}
	cfgPath := ProjectLauncherConfigPath(workdir)
	cfgHash, err := fileDigestOrAbsent(cfgPath)
	if err != nil {
		return "", err
	}
	profHash := "none"
	if sel.Layer == "workdir" && sel.Path != "" {
		profHash, err = fileDigestOrAbsent(sel.Path)
		if err != nil {
			return "", err
		}
	}
	if cfgHash == "absent" && profHash == "none" {
		return "", nil
	}
	sum := sha256.Sum256([]byte("config\x00" + cfgHash + "\x00profile\x00" + profHash))
	return hex.EncodeToString(sum[:]), nil
}

func fileDigestOrAbsent(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ProjectSandboxTrust reports whether a project's local sandbox content matches
// the approved pin for workdir. firstUse is true when no pin exists yet (the
// caller pins the current content). A missing store or corrupt entry degrades
// to first-use so an upgrade is not bricked, and the mismatch reason is meant
// for the user.
func ProjectSandboxTrust(workdir string, sel ProfileSelection) (trusted, firstUse bool, reason string) {
	want, err := ProjectSandboxContentHash(workdir, sel)
	if err != nil {
		return false, false, err.Error()
	}
	if want == "" {
		return true, false, "" // nothing local to pin or distrust
	}
	pins, err := loadProjectPins()
	if err != nil {
		return true, true, "" // store unreadable: treat as first use, warn silently
	}
	pin, ok := pins.Projects[workdir]
	if !ok || strings.TrimSpace(pin.Hash) == "" {
		return true, true, ""
	}
	if pin.Hash == want {
		return true, false, ""
	}
	return false, false, fmt.Sprintf(
		"project sandbox configuration %s changed since it was approved", LocalConfigDir(workdir))
}

// PinProjectSandbox records the current project sandbox content as approved.
func PinProjectSandbox(workdir string, sel ProfileSelection) error {
	hash, err := ProjectSandboxContentHash(workdir, sel)
	if err != nil {
		return err
	}
	if hash == "" {
		return nil
	}
	pins, err := loadProjectPins()
	if err != nil {
		return err
	}
	if pins.Projects == nil {
		pins.Projects = map[string]projectPin{}
	}
	pins.Projects[workdir] = projectPin{Hash: hash}
	return saveProjectPins(pins)
}

func loadProjectPins() (projectPinsFile, error) {
	path := projectPinsPath()
	if path == "" {
		return projectPinsFile{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return projectPinsFile{}, nil
	}
	if err != nil {
		return projectPinsFile{}, err
	}
	var pins projectPinsFile
	if err := json.Unmarshal(data, &pins); err != nil {
		return projectPinsFile{}, err
	}
	return pins, nil
}

func saveProjectPins(pins projectPinsFile) error {
	path := projectPinsPath()
	if path == "" {
		return fmt.Errorf("no home directory for the project sandbox pin store")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
