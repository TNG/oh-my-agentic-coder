// Package registry manages the per-workdir .opencode/sidecar.json file.
//
// Writes are atomic (write-to-temp + rename) and serialized via a
// flock on sidecar.json.lock. Only declared secret names (never values)
// are recorded here.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// SchemaVersion is the current on-disk format version.
const SchemaVersion = 1

// Registry is the root object of sidecar.json.
type Registry struct {
	Version    int     `json:"version"`
	Registered []Entry `json:"registered"`
}

// Entry is one registered skill sidecar.
type Entry struct {
	Name                string    `json:"name"`
	SkillDir            string    `json:"skill_dir"`
	MetaHash            string    `json:"meta_hash"`
	RegisteredAt        time.Time `json:"registered_at"`
	DeclaredSecretNames []string  `json:"declared_secret_names,omitempty"`
}

// Path returns the registry file path for a given workdir.
func Path(workdir string) string {
	return filepath.Join(workdir, ".opencode", "sidecar.json")
}

// LockPath returns the flock path for a given workdir.
func LockPath(workdir string) string {
	return filepath.Join(workdir, ".opencode", "sidecar.json.lock")
}

// Load reads the registry at workdir. A missing file returns an empty registry.
func Load(workdir string) (*Registry, error) {
	p := Path(workdir)
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Registry{Version: SchemaVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var r Registry
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	if r.Version == 0 {
		r.Version = SchemaVersion
	}
	return &r, nil
}

// Save atomically writes the registry to disk. The caller should hold the
// workdir lock (see WithLock).
func Save(workdir string, r *Registry) error {
	if r.Version == 0 {
		r.Version = SchemaVersion
	}
	dir := filepath.Join(workdir, ".opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ensure .opencode dir: %w", err)
	}
	// Deterministic ordering for diffs.
	sort.SliceStable(r.Registered, func(i, j int) bool {
		return r.Registered[i].Name < r.Registered[j].Name
	})
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "sidecar.json.tmp-*")
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
		return fmt.Errorf("rename registry: %w", err)
	}
	return nil
}

// Find returns the entry for name (or nil) and its index (or -1).
func (r *Registry) Find(name string) (*Entry, int) {
	for i := range r.Registered {
		if r.Registered[i].Name == name {
			return &r.Registered[i], i
		}
	}
	return nil, -1
}

// Upsert inserts or updates an entry.
func (r *Registry) Upsert(e Entry) {
	if _, idx := r.Find(e.Name); idx >= 0 {
		r.Registered[idx] = e
		return
	}
	r.Registered = append(r.Registered, e)
}

// Remove deletes an entry by name. Returns true if something was removed.
func (r *Registry) Remove(name string) bool {
	_, idx := r.Find(name)
	if idx < 0 {
		return false
	}
	r.Registered = append(r.Registered[:idx], r.Registered[idx+1:]...)
	return true
}

// WithLock acquires an exclusive flock on sidecar.json.lock for the
// duration of fn. The lock file is created 0600.
func WithLock(workdir string, fn func() error) error {
	dir := filepath.Join(workdir, ".opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ensure .opencode dir: %w", err)
	}
	f, err := os.OpenFile(LockPath(workdir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}
