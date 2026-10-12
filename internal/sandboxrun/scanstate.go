package sandboxrun

// This file keeps what the secret file scan (filesystem.deny, .env, ...)
// remembers across launches, in <profile>.scan.json next to the sandbox
// profile (e.g. ~/.config/omac/sandbox-profiles/default.scan.json):
//
//	{"schema": 1,
//	 "accepted_incomplete": [{"path": "/home/me/data", "accepted_at": "..."}],
//	 "known_secrets": ["/home/me/app/secrets/.env"]}
//
// accepted_incomplete are the folders the user answered "a" (always) for
// when the scan stopped early in them; launch no longer asks about them.
// known_secrets are files earlier scans hid. Launch keeps hiding them in
// a folder whose scan stops early, so an agent that floods a project with
// files cannot push an existing .env past the cap and unhide it.
//
// It is a separate file, not a profile field, so an older omac (which
// rejects unknown profile fields) still loads the profile, and so the
// profile the user edits is never rewritten. ~/.config/omac is hidden from
// the sandbox (baseline protected paths), so the agent cannot forge an
// acceptance. A missing file means nothing is accepted or known, which is
// how omac behaved before this file existed.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// scanStateSchema is the only schema version this omac reads or writes.
const scanStateSchema = 1

type acceptedFolder struct {
	Path       string    `json:"path"`
	AcceptedAt time.Time `json:"accepted_at"`
}

type scanStateFile struct {
	Schema             int              `json:"schema"`
	AcceptedIncomplete []acceptedFolder `json:"accepted_incomplete,omitempty"`
	KnownSecrets       []string         `json:"known_secrets,omitempty"`
}

// scanState is the loaded file. With path "" it is empty and read-only:
// no profile file, or a file this omac cannot read (see loadScanState).
type scanState struct {
	path   string
	file   scanStateFile
	accept []string // folders accepted this launch, written by save
}

// loadScanState reads the file at path. A missing file is an empty
// state. A file that cannot be parsed or has another schema (written by a
// newer omac) returns an error; the caller then uses an empty read-only
// state, so launch asks as before and the file is left alone.
func loadScanState(path string) (*scanState, error) {
	if path == "" {
		return &scanState{}, nil
	}
	f, err := readScanStateFile(path)
	if err != nil {
		return &scanState{}, err
	}
	return &scanState{path: path, file: f}, nil
}

func readScanStateFile(path string) (scanStateFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return scanStateFile{Schema: scanStateSchema}, nil
	}
	if err != nil {
		return scanStateFile{}, fmt.Errorf("read %s: %w", path, err)
	}
	var f scanStateFile
	if err := json.Unmarshal(data, &f); err != nil {
		return scanStateFile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Schema != scanStateSchema {
		return scanStateFile{}, fmt.Errorf("%s: unsupported schema %d", path, f.Schema)
	}
	return f, nil
}

// canSave reports whether an "always" answer can be stored.
func (s *scanState) canSave() bool { return s.path != "" }

// isAccepted reports whether the user answered "always" for root earlier.
func (s *scanState) isAccepted(root string) bool {
	for _, a := range s.file.AcceptedIncomplete {
		if samePath(a.Path, root) {
			return true
		}
	}
	return false
}

// acceptAlways marks roots as accepted; save writes them.
func (s *scanState) acceptAlways(roots []string) {
	s.accept = append(s.accept, roots...)
}

func (s *scanState) knownSecrets() []string { return s.file.KnownSecrets }

// save writes the folders accepted this launch and the secret files this
// launch hid. scannedRoots are the folders this launch scanned: known
// secrets inside them are replaced by found (a file deleted since is
// dropped), entries elsewhere (other projects) are kept. The file is
// re-read under a lock so two launches at once do not drop each other's
// entries, and it is only written when something changed.
func (s *scanState) save(scannedRoots, found []string) error {
	if s.path == "" {
		return nil
	}
	dir := filepath.Dir(s.path)
	return registry.WithLockAt(dir, s.path+".lock", func() error {
		cur, err := readScanStateFile(s.path)
		if err != nil {
			return err // never overwrite a file we cannot read
		}
		next := cur
		next.AcceptedIncomplete = slices.Clone(cur.AcceptedIncomplete)
		for _, r := range s.accept {
			if !slices.ContainsFunc(next.AcceptedIncomplete, func(a acceptedFolder) bool { return samePath(a.Path, r) }) {
				next.AcceptedIncomplete = append(next.AcceptedIncomplete, acceptedFolder{Path: r, AcceptedAt: time.Now().UTC()})
			}
		}
		var known []string
		for _, k := range cur.KnownSecrets {
			if !withinAny(k, scannedRoots, pathForms) {
				known = append(known, k)
			}
		}
		next.KnownSecrets = dedupe(append(known, found...))
		if len(next.AcceptedIncomplete) == len(cur.AcceptedIncomplete) && slices.Equal(next.KnownSecrets, dedupe(cur.KnownSecrets)) {
			return nil
		}
		data, err := sandboxprofile.MarshalPretty(next)
		if err != nil {
			return err
		}
		return sandboxprofile.WriteFileAtomic(s.path, data)
	})
}

// samePath compares two paths by their written and symlink-resolved
// spellings.
func samePath(a, b string) bool {
	for _, fa := range pathForms(a) {
		if slices.Contains(pathForms(b), fa) {
			return true
		}
	}
	return false
}
