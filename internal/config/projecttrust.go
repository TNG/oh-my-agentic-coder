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
// a launch actually loads is pinned per file under ~/.config/omac (host-only,
// invisible to the sandbox) once a human approved it, and a later launch
// aborts when a loaded file no longer matches its pin.

// projectPinsFile is the host-only store of approved project sandbox content.
type projectPinsFile struct {
	Projects map[string]projectPin `json:"projects"`
}

// projectPin holds the approved digest per loaded file for one workdir. An
// empty hash means that slot was never covered by an approval — which also
// means pins written by earlier builds of this feature (single combined hash)
// count as unapproved and ask for one re-approval.
type projectPin struct {
	ConfigHash  string `json:"config_hash,omitempty"`
	ProfileHash string `json:"profile_hash,omitempty"`
}

func projectPinsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "omac", "project-sandbox.json")
}

type fileKind int

const (
	fileConfig fileKind = iota
	fileProfile
)

// loadedFile is one project-local artifact a launch loads: its path, digest,
// and the pin slot that covers it.
type loadedFile struct {
	path   string
	digest string
	kind   fileKind
}

// loadedProjectFiles collects the project-local files the selection actually
// loads, with their digests. configDriven is false for an explicit
// --profile-path: the command line bypasses .omac/config.yaml, so that file is
// not part of what the launch reads. An empty result means the launch carries
// no project-local content.
func loadedProjectFiles(workdir string, sel ProfileSelection, configDriven bool) ([]loadedFile, error) {
	if workdir == "" {
		return nil, nil
	}
	var files []loadedFile
	if configDriven {
		digest, err := fileDigest(ProjectLauncherConfigPath(workdir))
		if err != nil {
			return nil, err
		}
		if digest != "" {
			files = append(files, loadedFile{path: ProjectLauncherConfigPath(workdir), digest: digest, kind: fileConfig})
		}
	}
	if sel.Layer == "workdir" && sel.Path != "" {
		digest, err := fileDigest(sel.Path)
		if err != nil {
			return nil, err
		}
		if digest == "" {
			// The selection validated existence moments ago; a missing file
			// here is a deletion racing the launch, not an absent-by-design.
			return nil, fmt.Errorf("read %s: file disappeared between validation and digesting", sel.Path)
		}
		files = append(files, loadedFile{path: sel.Path, digest: digest, kind: fileProfile})
	}
	return files, nil
}

// fileDigest hashes a file; a missing file hashes as "".
func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ProjectSandboxTrust reports whether the project-local content this selection
// loads matches the approved pin for workdir.
//
// trusted means the launch may proceed. firstUse is true when relevant content
// exists but no approval covers any of it — the caller decides whether that is
// approval-free (explicit --profile-path) or requires --accept-project-config.
// Only the loaded files are compared, and PinProjectSandbox only writes the
// loaded files' slots, so an approval never covers content it did not see; a
// slot pinned under another path (config-driven approval of the current
// config, seen by today's explicit-profile launch) stays untouched. A missing
// store or a pin without per-file hashes degrades to unapproved. The reason
// phrases are user-facing.
func ProjectSandboxTrust(workdir string, sel ProfileSelection, configDriven bool) (trusted, firstUse bool, reason string) {
	files, err := loadedProjectFiles(workdir, sel, configDriven)
	if err != nil {
		return false, false, err.Error()
	}
	if len(files) == 0 {
		return true, false, "" // nothing project-local is loaded
	}
	pins, err := loadProjectPins()
	pin := projectPin{}
	if err == nil {
		pin = pins.Projects[workdir]
	}
	var changed, unapproved []string
	for _, f := range files {
		pinned := pin.ProfileHash
		if f.kind == fileConfig {
			pinned = pin.ConfigHash
		}
		switch {
		case pinned == "":
			unapproved = append(unapproved, f.path)
		case pinned != f.digest:
			changed = append(changed, f.path)
		}
	}
	switch {
	case len(changed) == 0 && len(unapproved) == 0:
		return true, false, ""
	case len(changed) == 0:
		return false, true, strings.Join(unapproved, ", ") + " present but not yet approved"
	default:
		reason := strings.Join(changed, " and ") + " changed since it was approved"
		if len(unapproved) > 0 {
			reason += "; " + strings.Join(unapproved, ", ") + " not yet approved"
		}
		return false, false, reason
	}
}

// PinProjectSandbox records the project-local content this selection loads as
// approved, per file.
func PinProjectSandbox(workdir string, sel ProfileSelection, configDriven bool) error {
	files, err := loadedProjectFiles(workdir, sel, configDriven)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	pins, err := loadProjectPins()
	if err != nil {
		return err
	}
	if pins.Projects == nil {
		pins.Projects = map[string]projectPin{}
	}
	pin := pins.Projects[workdir]
	for _, f := range files {
		if f.kind == fileConfig {
			pin.ConfigHash = f.digest
		} else {
			pin.ProfileHash = f.digest
		}
	}
	pins.Projects[workdir] = pin
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
