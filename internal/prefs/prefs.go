// Package prefs manages user-global, non-secret omac preferences — the
// small set of "remember my choice" flags that are not tied to any single
// skill or workdir.
//
// It holds SuppressPluginWarning, the persisted answer to the "do not warn
// me again" option of the missing-plugin warning that `omac serve` shows when
// the OpenCode Desktop multidir plugin is not installed in a workdir, and
// ProtectedWatchIgnore, the exact paths the user dismissed in the mid-session
// protected-file dialog.
//
// The file lives next to the other global omac state
// (~/.config/omac/prefs.yaml, honoring $XDG_CONFIG_HOME) and is written
// atomically (temp-file + rename, mode 0600), mirroring
// internal/skillconfig.
package prefs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the current on-disk format version.
const SchemaVersion = 1

// Store is the root object of prefs.yaml.
type Store struct {
	Version int `yaml:"version"`

	// SuppressPluginWarning, when true, silences the `omac serve`
	// warning that the OpenCode Desktop multidir plugin is not installed
	// in the active workdir. Set by choosing "do not warn me again" at
	// the warning prompt.
	SuppressPluginWarning bool `yaml:"suppress_plugin_warning"`

	// ProtectedWatchIgnore lists absolute paths whose mid-session
	// protected-file notice the user dismissed with "ignore
	// permanently". The paths stay protected at launch; only the
	// repeated warning for an already-reviewed file is skipped.
	ProtectedWatchIgnore []string `yaml:"protected_watch_ignore"`
}

// IgnoresProtectedPath reports whether the exact path was dismissed by the
// user in the mid-session watch.
func (s *Store) IgnoresProtectedPath(path string) bool {
	if s == nil {
		return false
	}
	path = filepath.Clean(path)
	for _, p := range s.ProtectedWatchIgnore {
		if filepath.Clean(p) == path {
			return true
		}
	}
	return false
}

// AddProtectedWatchIgnore records path as permanently ignored and persists the
// store. A path already in the list is a no-op.
func (s *Store) AddProtectedWatchIgnore(path string) error {
	if s == nil {
		return fmt.Errorf("prefs: nil store")
	}
	path = filepath.Clean(path)
	if s.IgnoresProtectedPath(path) {
		return nil
	}
	s.ProtectedWatchIgnore = append(s.ProtectedWatchIgnore, path)
	return Save(s)
}

// GlobalDir returns the directory holding user-global omac preferences.
// It honors $XDG_CONFIG_HOME (falling back to $HOME/.config) with an
// "omac" leaf, matching the launcher/skill-config global location. An
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

// Path returns the user-global prefs file path, or "" when no global
// directory can be resolved.
func Path() string {
	dir := GlobalDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "prefs.yaml")
}

// Load reads the user-global prefs. A missing file (or an unresolvable
// global directory) returns an empty Store, never an error, so callers
// can treat "no prefs yet" the same as "all defaults".
func Load() (*Store, error) {
	p := Path()
	if p == "" {
		return &Store{Version: SchemaVersion}, nil
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Store{Version: SchemaVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read prefs: %w", err)
	}
	var s Store
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse prefs: %w", err)
	}
	if s.Version == 0 {
		s.Version = SchemaVersion
	}
	return &s, nil
}

// Save atomically writes the prefs to disk. Returns an error when no
// global directory can be resolved.
func Save(s *Store) error {
	dir := GlobalDir()
	if dir == "" {
		return fmt.Errorf("prefs: no global config directory available (set $HOME or $XDG_CONFIG_HOME)")
	}
	if s.Version == 0 {
		s.Version = SchemaVersion
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ensure config dir: %w", err)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(s); err != nil {
		_ = enc.Close()
		return fmt.Errorf("marshal prefs: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close yaml encoder: %w", err)
	}
	data := buf.Bytes()

	finalPath := filepath.Join(dir, "prefs.yaml")
	tmp, err := os.CreateTemp(dir, "prefs.yaml.tmp-*")
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
		return fmt.Errorf("rename prefs: %w", err)
	}
	return nil
}
