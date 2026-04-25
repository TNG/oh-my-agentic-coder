// Package skillconfig manages the per-workdir
// .opencode/skill-config.json file. This is the home of non-secret
// skill configuration (API base URLs, region names, feature flags,
// retry limits — anything that wouldn't be embarrassing in a
// screenshot).
//
// Secret credentials must continue to use internal/keychain. The
// skill-config file is plain JSON, mode 0600, and is meant to be
// readable by the user (and committable to a private workdir if they
// choose) — it is NOT a secret store.
//
// Writes are atomic (write-to-temp + rename) and serialized via the
// same flock that protects sidecar.json — see registry.WithLock —
// because both files belong to the same .opencode/ directory.
package skillconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// SchemaVersion is the current on-disk format version.
const SchemaVersion = 1

// Store is the root object of skill-config.json. The map is keyed by
// skill name; each value is keyed by field name and stores the
// canonical string form of the field's value (the type is recovered
// from meta.yaml at start time).
type Store struct {
	Version int                          `json:"version"`
	Skills  map[string]map[string]string `json:"skills"`
}

// Path returns the skill-config file path for a given workdir.
func Path(workdir string) string {
	return filepath.Join(workdir, ".opencode", "skill-config.json")
}

// Load reads the file at workdir. A missing file returns an empty Store.
func Load(workdir string) (*Store, error) {
	p := Path(workdir)
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Store{Version: SchemaVersion, Skills: map[string]map[string]string{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read skill-config: %w", err)
	}
	var s Store
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse skill-config: %w", err)
	}
	if s.Version == 0 {
		s.Version = SchemaVersion
	}
	if s.Skills == nil {
		s.Skills = map[string]map[string]string{}
	}
	return &s, nil
}

// Save atomically writes the store to disk. The caller should hold the
// workdir lock (registry.WithLock).
func Save(workdir string, s *Store) error {
	if s.Version == 0 {
		s.Version = SchemaVersion
	}
	if s.Skills == nil {
		s.Skills = map[string]map[string]string{}
	}
	dir := filepath.Join(workdir, ".opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ensure .opencode dir: %w", err)
	}

	// Marshal with deterministic key ordering so diffs are clean.
	// encoding/json's encoder sorts map keys alphabetically already
	// (since Go 1.12), so MarshalIndent on Store gives stable output.
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal skill-config: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "skill-config.json.tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, Path(workdir)); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename skill-config: %w", err)
	}
	return nil
}

// Get returns the stored value for a (skill, field) pair, plus whether
// it was present. An absent skill or field both return ok=false.
func (s *Store) Get(skill, field string) (string, bool) {
	if m, ok := s.Skills[skill]; ok {
		v, present := m[field]
		return v, present
	}
	return "", false
}

// Set stores a value for a (skill, field) pair, creating the per-skill
// map if necessary.
func (s *Store) Set(skill, field, value string) {
	if s.Skills == nil {
		s.Skills = map[string]map[string]string{}
	}
	m, ok := s.Skills[skill]
	if !ok {
		m = map[string]string{}
		s.Skills[skill] = m
	}
	m[field] = value
}

// Unset removes a single field. Returns true if something was removed.
// Removes the skill entry entirely if it becomes empty.
func (s *Store) Unset(skill, field string) bool {
	m, ok := s.Skills[skill]
	if !ok {
		return false
	}
	if _, present := m[field]; !present {
		return false
	}
	delete(m, field)
	if len(m) == 0 {
		delete(s.Skills, skill)
	}
	return true
}

// RemoveSkill drops every field stored for a skill. Used by deregister.
// Returns true if the skill had any entries.
func (s *Store) RemoveSkill(skill string) bool {
	if _, ok := s.Skills[skill]; !ok {
		return false
	}
	delete(s.Skills, skill)
	return true
}

// FieldsFor returns a sorted shallow copy of the fields stored for skill.
// Returns nil for an unknown skill.
func (s *Store) FieldsFor(skill string) []string {
	m, ok := s.Skills[skill]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
