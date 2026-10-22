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

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
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

// projectPin holds the approved digest per project-local file path for one
// workdir. A missing key means the file was never approved; an approved
// absent file is recorded with a digest of "". Pins written by earlier
// builds of this feature (per-file-name slots, single combined hash before
// that) carry no per-path entries, so they count as unapproved and ask for
// one re-approval.
type projectPin struct {
	Files map[string]string `json:"files"`
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

// loadedFile is one project-local artifact a launch loads, with the digest of
// its current on-disk content. An absent file keeps the empty digest: the
// pin decides whether an absence was approved.
type loadedFile struct {
	path   string
	digest string
	kind   fileKind
}

// loadedProjectFiles collects the project-local files the selection loads,
// with their digests. configDriven is false for an explicit --profile-path:
// the command line bypasses .omac/config.yaml, so that file is not part of
// what the launch reads. The profile file is loaded whenever the selection
// names a workdir-layer file — including one that vanished after validation,
// whose absence is then judged against its pin like any other change.
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
		files = append(files, loadedFile{path: ProjectLauncherConfigPath(workdir), digest: digest, kind: fileConfig})
	}
	if sel.Layer == "workdir" && sel.Path != "" {
		digest, err := fileDigest(sel.Path)
		if err != nil {
			return nil, err
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
// An error return means the loaded files could not be read (or their state
// could not be established): the caller must refuse the launch without the
// re-approval guidance — nothing changed, so re-approving would record
// nothing. Otherwise:
//
//   - trusted means every loaded file matches its pin (or is absent and was
//     never approved), and the launch may proceed;
//   - firstUse means some loaded file has content but no approval covers it —
//     the caller decides whether that is approval-free (explicit
//     --profile-path) or requires --accept-project-config;
//   - neither means a loaded file changed or disappeared since its approval,
//     or unapproved content appeared on top of approved content.
//
// pinning records per path, so an approval and a later comparison always
// speak about the same file, and approving profile B never disturbs the
// pin that covered profile A. The reason phrases are user-facing.
func ProjectSandboxTrust(workdir string, sel ProfileSelection, configDriven bool) (trusted, firstUse bool, reason string, err error) {
	files, err := loadedProjectFiles(workdir, sel, configDriven)
	if err != nil {
		return false, false, "", err
	}
	pins, loadErr := loadProjectPins()
	if loadErr != nil {
		return false, false, "", fmt.Errorf("read the approved project sandbox store (%s): %w", projectPinsPath(), loadErr)
	}
	pin := pins.Projects[workdir]
	var changed, unapproved []string
	for _, f := range files {
		approved, pinned := pin.Files[f.path]
		switch {
		case !pinned && f.digest != "":
			unapproved = append(unapproved, f.path)
		case !pinned:
			continue
		case f.digest != approved:
			if approved != "" && f.digest == "" {
				changed = append(changed, fmt.Sprintf("%s has disappeared since it was approved", f.path))
			} else {
				changed = append(changed, fmt.Sprintf("%s changed since it was approved", f.path))
			}
		}
	}
	switch {
	case len(changed) == 0 && len(unapproved) == 0:
		return true, false, "", nil
	case len(changed) == 0:
		return false, true, strings.Join(unapproved, ", ") + " present but not yet approved", nil
	default:
		reason := strings.Join(changed, " and ")
		if len(unapproved) > 0 {
			reason += "; " + strings.Join(unapproved, ", ") + " not yet approved"
		}
		return false, false, reason, nil
	}
}

// PinProjectSandbox records the project-local content this selection loads as
// approved, one entry per file path. Under the store's flock, so two
// concurrent approvals cannot lose each other's entries. A file that has
// already a pin but vanished (re-approving an absence) is recorded as
// approved-absent; a file that is absent and was never approved is not an
// approval subject at all.
func PinProjectSandbox(workdir string, sel ProfileSelection, configDriven bool) error {
	files, err := loadedProjectFiles(workdir, sel, configDriven)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	unlock, err := lockProjectPins()
	if err != nil {
		return err
	}
	defer unlock()
	pins, err := loadProjectPins()
	if err != nil {
		return err
	}
	if pins.Projects == nil {
		pins.Projects = map[string]projectPin{}
	}
	pin := pins.Projects[workdir]
	if pin.Files == nil {
		pin.Files = map[string]string{}
	}
	for _, f := range files {
		if f.digest == "" {
			if _, pinned := pin.Files[f.path]; pinned {
				pin.Files[f.path] = "" // re-approved as absent
			}
			continue
		}
		pin.Files[f.path] = f.digest
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

func marshalProjectPins(pins projectPinsFile) ([]byte, error) {
	data, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func saveProjectPins(pins projectPinsFile) error {
	path := projectPinsPath()
	if path == "" {
		return fmt.Errorf("no home directory for the project sandbox pin store")
	}
	data, err := marshalProjectPins(pins)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return sandboxprofile.WriteFileAtomic(path, data, 0o600)
}
