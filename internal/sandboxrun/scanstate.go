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
// files cannot push an existing .env past the cap and unhide it. If one
// of them is gone there (e.g. the agent renamed its folder), launch asks
// again, even for an accepted folder.
//
// Both lists store paths with symlinks resolved, once, when they are
// written, and never resolve a stored path again. Otherwise an agent that
// replaces an accepted folder by a symlink to another folder would carry
// the acceptance over to it.
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
	"strings"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// scanStateSchema is the only schema version this omac reads or writes.
const scanStateSchema = 1

// maxKnownSecrets bounds known_secrets. Each scan can match up to
// maxDenyScanEntries files, and an agent can create many .env files, so
// without a bound the file and the launch-time checks could grow without
// limit. Entries stored earlier are kept first.
const maxKnownSecrets = 10000

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

// isAccepted reports whether the user answered "always" for root
// earlier. Only the exact folder counts, not a folder inside it.
func (s *scanState) isAccepted(root string) bool {
	r := resolvedPath(root)
	return slices.ContainsFunc(s.file.AcceptedIncomplete, func(a acceptedFolder) bool { return a.Path == r })
}

// acceptAlways marks roots as accepted; save writes them.
func (s *scanState) acceptAlways(roots []string) {
	for _, r := range roots {
		s.accept = append(s.accept, resolvedPath(r))
	}
}

func (s *scanState) knownSecrets() []string { return s.file.KnownSecrets }

// save writes the folders accepted this launch and what it learned about
// secret files. scannedRoots are the folders this launch scanned; stored
// known secrets inside them are replaced by found (files matched now) and
// kept (remembered files kept for a folder whose scan stopped early, see
// checkKnownSecrets). A remembered file in neither is gone and is
// dropped; save only runs once the user agreed to start. Entries outside
// scannedRoots (other projects) are left as they are.
//
// The file is re-read under a lock so two launches at once do not drop
// each other's entries. Nothing is locked or written when nothing changed.
func (s *scanState) save(scannedRoots, found, kept []string) error {
	if s.path == "" {
		return nil
	}
	var roots []string
	for _, r := range scannedRoots {
		roots = append(roots, resolvedPath(r))
	}
	merge := func(cur scanStateFile) (scanStateFile, bool) {
		next := cur
		next.AcceptedIncomplete = slices.Clone(cur.AcceptedIncomplete)
		for _, r := range s.accept {
			if !slices.ContainsFunc(next.AcceptedIncomplete, func(a acceptedFolder) bool { return a.Path == r }) {
				next.AcceptedIncomplete = append(next.AcceptedIncomplete, acceptedFolder{Path: r, AcceptedAt: time.Now().UTC()})
			}
		}
		next.KnownSecrets = mergeKnownSecrets(cur.KnownSecrets, roots, found, kept)
		changed := len(next.AcceptedIncomplete) != len(cur.AcceptedIncomplete) || !slices.Equal(next.KnownSecrets, cur.KnownSecrets)
		return next, changed
	}
	if _, changed := merge(s.file); !changed {
		return nil
	}
	return registry.WithLockAt(filepath.Dir(s.path), s.path+".lock", func() error {
		cur, err := readScanStateFile(s.path)
		if err != nil {
			return err // never overwrite a file we cannot read
		}
		next, changed := merge(cur)
		if !changed {
			return nil
		}
		data, err := sandboxprofile.MarshalPretty(next)
		if err != nil {
			return err
		}
		return sandboxprofile.WriteFileAtomic(s.path, data)
	})
}

// mergeKnownSecrets returns the new known_secrets list, sorted: stored
// entries outside roots, kept, and found. Past maxKnownSecrets, entries
// already stored win over new ones, so an agent cannot push the user's
// real .env files out of the list by creating many new ones.
func mergeKnownSecrets(stored, roots, found, kept []string) []string {
	sep := string(filepath.Separator)
	inRoots := func(k string) bool {
		return slices.ContainsFunc(roots, func(r string) bool {
			return strings.HasPrefix(k, strings.TrimSuffix(r, sep)+sep)
		})
	}
	was := make(map[string]bool, len(stored))
	for _, k := range stored {
		was[k] = true
	}
	var old, added []string
	for _, k := range stored {
		if !inRoots(k) {
			old = append(old, k)
		}
	}
	old = append(old, kept...)
	for _, f := range found {
		if was[f] {
			old = append(old, f)
		} else {
			added = append(added, f)
		}
	}
	out := dedupe(old)
	for _, a := range dedupe(added) {
		if len(out) >= maxKnownSecrets {
			break
		}
		out = append(out, a)
	}
	if len(out) > maxKnownSecrets {
		out = out[:maxKnownSecrets]
	}
	slices.Sort(out)
	return out
}

// resolvedPath returns p with symlinks resolved, or p when that fails.
func resolvedPath(p string) string {
	if r := canonicalPath(p); r != "" {
		return r
	}
	return p
}
