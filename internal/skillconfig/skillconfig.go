// Package skillconfig manages the per-workdir
// .opencode/skill-config.yaml file. This is the home of non-secret
// skill configuration (API base URLs, region names, feature flags,
// retry limits — anything that wouldn't be embarrassing in a
// screenshot).
//
// Secret credentials must continue to use internal/keychain. The
// skill-config file is plain YAML, mode 0600, and is meant to be
// readable by the user (and committable to a private workdir if they
// choose) — it is NOT a secret store.
//
// Writes are atomic (write-to-temp + rename) and serialized via the
// same flock that protects sidecar.json — see registry.WithLock —
// because both files belong to the same .opencode/ directory.
package skillconfig

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the current on-disk format version.
const SchemaVersion = 1

// Store is the root object of skill-config.yaml. The map is keyed by
// skill name; each value is keyed by field name and stores the
// canonical string form of the field's value (the type is recovered
// from omac.yaml at start time).
type Store struct {
	Version int                          `yaml:"version"`
	Skills  map[string]map[string]string `yaml:"skills"`
	// Defaults holds "last-known-good" config values keyed by
	// skill→field, mirrored on every write so a future `register
	// --defaults` elsewhere can reuse them as suggestions
	// (docs/MULTI_DIR_DESKTOP.md §4.4). Only meaningful in the global
	// store; never consulted at runtime (runtime uses Skills only).
	Defaults map[string]map[string]string `yaml:"defaults,omitempty"`
}

// Path returns the skill-config file path for a given workdir.
func Path(workdir string) string {
	return filepath.Join(workdir, ".opencode", "skill-config.yaml")
}

// GlobalDir returns the directory holding user-global skill config. It
// honors $XDG_CONFIG_HOME (falling back to $HOME/.config) with an
// "omac" leaf, matching the launcher config's global location. An
// empty string means no global directory could be resolved.
func GlobalDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "omac")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "omac")
}

// GlobalPath returns the user-global skill-config file path, or "" when
// no global directory can be resolved.
func GlobalPath() string {
	dir := GlobalDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "skill-config.yaml")
}

// Load reads the file at workdir. A missing file returns an empty Store.
func Load(workdir string) (*Store, error) {
	return loadFrom(Path(workdir))
}

// LoadGlobal reads the user-global store. A missing file (or an
// unresolvable global directory) returns an empty Store.
func LoadGlobal() (*Store, error) {
	p := GlobalPath()
	if p == "" {
		return &Store{Version: SchemaVersion, Skills: map[string]map[string]string{}}, nil
	}
	return loadFrom(p)
}

// loadFrom reads and parses a store from an arbitrary path.
func loadFrom(p string) (*Store, error) {
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Store{Version: SchemaVersion, Skills: map[string]map[string]string{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read skill-config: %w", err)
	}
	var s Store
	if err := yaml.Unmarshal(raw, &s); err != nil {
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
	return saveTo(filepath.Join(workdir, ".opencode"), Path(workdir), s)
}

// SaveGlobal atomically writes the user-global store. The caller should
// hold the global lock (registry.WithGlobalLock). Returns an error when
// no global directory can be resolved.
func SaveGlobal(s *Store) error {
	dir := GlobalDir()
	if dir == "" {
		return fmt.Errorf("skill-config: no global config directory available (set $HOME or $XDG_CONFIG_HOME)")
	}
	return saveTo(dir, filepath.Join(dir, "skill-config.yaml"), s)
}

// saveTo atomically writes s to finalPath, creating dir (0700) first.
func saveTo(dir, finalPath string, s *Store) error {
	if s.Version == 0 {
		s.Version = SchemaVersion
	}
	if s.Skills == nil {
		s.Skills = map[string]map[string]string{}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ensure config dir: %w", err)
	}

	// yaml.v3's encoder sorts map keys alphabetically by default, so
	// MarshalIndent gives stable output across runs and across Go
	// versions. Encode with a 2-space indent for visual parity with
	// hand-edited files.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(s); err != nil {
		_ = enc.Close()
		return fmt.Errorf("marshal skill-config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close yaml encoder: %w", err)
	}
	data := buf.Bytes()

	tmp, err := os.CreateTemp(dir, "skill-config.yaml.tmp-*")
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
	if err := os.Rename(tmpPath, finalPath); err != nil {
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

// GetDefault returns the remembered default for a (skill, field), plus
// whether present (docs/MULTI_DIR_DESKTOP.md §4.4).
func (s *Store) GetDefault(skill, field string) (string, bool) {
	if m, ok := s.Defaults[skill]; ok {
		v, present := m[field]
		return v, present
	}
	return "", false
}

// SetDefault records a remembered default for a (skill, field).
func (s *Store) SetDefault(skill, field, value string) {
	if s.Defaults == nil {
		s.Defaults = map[string]map[string]string{}
	}
	m, ok := s.Defaults[skill]
	if !ok {
		m = map[string]string{}
		s.Defaults[skill] = m
	}
	m[field] = value
}

// RemoveDefaults drops every remembered default for a skill. Used by
// `deregister --purge-defaults`. Returns true if anything was removed.
func (s *Store) RemoveDefaults(skill string) bool {
	if _, ok := s.Defaults[skill]; !ok {
		return false
	}
	delete(s.Defaults, skill)
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
