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
	"strings"

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

	// ProtectedWatchProjectIgnores lists basenames the user dismissed for
	// one project. The root is the canonical git root shared by every
	// worktree of that repository, so a new worktree under a new path is
	// covered. The file stays protected at launch.
	ProtectedWatchProjectIgnores []ProtectedWatchProjectIgnore `yaml:"protected_watch_project_ignores"`
}

// ProtectedWatchProjectIgnore is one project-scoped protected-file exception:
// every file named Name below Root is skipped by the mid-session notice.
type ProtectedWatchProjectIgnore struct {
	Root string `yaml:"root"`
	Name string `yaml:"name"`
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

// IgnoresProtectedProjectPath reports whether path matches one of the stored
// project-scoped exceptions. It is separate from IgnoresProtectedPath so the
// exact-path wording and behavior stay unchanged.
func (s *Store) IgnoresProtectedProjectPath(path string) bool {
	if s == nil {
		return false
	}
	for _, ignore := range s.ProtectedWatchProjectIgnores {
		if MatchesProjectIgnore(ignore.Root, ignore.Name, path) {
			return true
		}
	}
	return false
}

// AddProtectedWatchProjectIgnore records that every file named name below root
// is ignored and persists the store. A pair already in the list is a no-op.
func (s *Store) AddProtectedWatchProjectIgnore(root, name string) error {
	if s == nil {
		return fmt.Errorf("prefs: nil store")
	}
	root = canonicalPath(root)
	if root == "" || !validProjectName(name) {
		return fmt.Errorf("prefs: invalid project ignore root=%q name=%q", root, name)
	}
	for _, ignore := range s.ProtectedWatchProjectIgnores {
		if ignore.Root == root && ignore.Name == name {
			return nil
		}
	}
	s.ProtectedWatchProjectIgnores = append(s.ProtectedWatchProjectIgnores, ProtectedWatchProjectIgnore{
		Root: root,
		Name: name,
	})
	return Save(s)
}

// MatchesProjectIgnore reports whether path is a file named name below root.
// Both sides are canonicalised, so a symlinked worktree matches the root it
// really lives in, and the separator boundary keeps /repo-other out of /repo.
func MatchesProjectIgnore(root, name, path string) bool {
	if root == "" || !validProjectName(name) {
		return false
	}
	path = canonicalPath(path)
	if filepath.Base(path) != name {
		return false
	}
	root = canonicalPath(root)
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validProjectName accepts a plain file basename, not a path fragment.
func validProjectName(name string) bool {
	return name != "" && name != "." && name != ".." && name == filepath.Base(name)
}

// canonicalPath resolves symlinks in the deepest existing ancestor and
// re-grafts the remaining tail, so a path that does not exist yet still maps
// /var/... to /private/var/... on macOS. It returns "" for an empty input.
func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	dir, tail := filepath.Dir(path), filepath.Base(path)
	for dir != "/" && dir != "." {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, tail)
		}
		dir, tail = filepath.Dir(dir), filepath.Join(filepath.Base(dir), tail)
	}
	return path
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
