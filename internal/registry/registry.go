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
//
// BundleHash covers every meaningful file in the skill directory
// (omac.yaml plus the sidecar source), excluding runtime artifacts
// and developer caches. See config.BundleHash for the wire format.
// It supersedes the older meta_hash that only covered omac.yaml,
// so an install-script edit now invalidates the registration too.
type Entry struct {
	Name                string    `json:"name"`
	SkillDir            string    `json:"skill_dir"`
	BundleHash          string    `json:"bundle_hash"`
	RegisteredAt        time.Time `json:"registered_at"`
	DeclaredSecretNames []string  `json:"declared_secret_names,omitempty"`
	// SkippedSecretNames records optional secrets the user explicitly
	// declined to provide at register time (entered an empty value at
	// the prompt). Re-registration must NOT re-ask for these unless
	// --reprompt-secrets is passed; otherwise every `omac register
	// --force <skill>` would force the user to mash Enter through every
	// optional secret again.
	SkippedSecretNames []string `json:"skipped_secret_names,omitempty"`
	// SkippedConfigFields is the analogous list for non-secret config
	// fields (skill-config.yaml). Same rationale as
	// SkippedSecretNames; cleared by --reprompt-fields.
	SkippedConfigFields []string `json:"skipped_config_fields,omitempty"`
}

// Path returns the registry file path for a given workdir.
func Path(workdir string) string {
	return filepath.Join(workdir, ".opencode", "sidecar.json")
}

// LockPath returns the flock path for a given workdir.
func LockPath(workdir string) string {
	return filepath.Join(workdir, ".opencode", "sidecar.json.lock")
}

// GlobalDir returns the directory that holds user-global registration
// state. It honors $XDG_CONFIG_HOME when set (per the XDG Base
// Directory spec), falling back to $HOME/.config. The leaf is
// "omac" to match the launcher config's global location
// (~/.config/omac/config.yaml; see config.LoadLauncher).
//
// An empty string is returned when neither XDG_CONFIG_HOME nor a home
// directory can be determined; callers should treat that as "no
// global store available" and fall back to workdir-only behavior.
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

// GlobalPath returns the user-global registry file path, or "" when no
// global directory can be resolved (see GlobalDir).
func GlobalPath() string {
	dir := GlobalDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "sidecar.json")
}

// GlobalLockPath returns the flock path guarding the global registry,
// or "" when no global directory can be resolved.
func GlobalLockPath() string {
	dir := GlobalDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "sidecar.json.lock")
}

// Load reads the registry at workdir. A missing file returns an empty registry.
func Load(workdir string) (*Registry, error) {
	return loadFrom(Path(workdir))
}

// LoadGlobal reads the user-global registry. A missing file (or an
// unresolvable global directory) returns an empty registry, so callers
// can always merge it in unconditionally.
func LoadGlobal() (*Registry, error) {
	p := GlobalPath()
	if p == "" {
		return &Registry{Version: SchemaVersion}, nil
	}
	return loadFrom(p)
}

// loadFrom reads and parses a registry file at an arbitrary path.
func loadFrom(p string) (*Registry, error) {
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
	return saveTo(filepath.Join(workdir, ".opencode"), Path(workdir), r)
}

// SaveGlobal atomically writes the user-global registry. The caller
// should hold the global lock (see WithGlobalLock). Returns an error
// when no global directory can be resolved.
func SaveGlobal(r *Registry) error {
	dir := GlobalDir()
	if dir == "" {
		return fmt.Errorf("registry: no global config directory available (set $HOME or $XDG_CONFIG_HOME)")
	}
	return saveTo(dir, filepath.Join(dir, "sidecar.json"), r)
}

// saveTo atomically writes r to finalPath, creating dir (0700) first.
func saveTo(dir, finalPath string, r *Registry) error {
	if r.Version == 0 {
		r.Version = SchemaVersion
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ensure registry dir: %w", err)
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
	if err := os.Rename(tmpPath, finalPath); err != nil {
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
	return withLockAt(filepath.Join(workdir, ".opencode"), LockPath(workdir), fn)
}

// WithGlobalLock acquires an exclusive flock on the user-global
// sidecar.json.lock for the duration of fn. Returns an error when no
// global directory can be resolved.
func WithGlobalLock(fn func() error) error {
	dir := GlobalDir()
	if dir == "" {
		return fmt.Errorf("registry: no global config directory available (set $HOME or $XDG_CONFIG_HOME)")
	}
	return withLockAt(dir, filepath.Join(dir, "sidecar.json.lock"), fn)
}

// withLockAt is the shared flock primitive: it ensures dir exists,
// acquires an exclusive lock on lockPath, and runs fn while held.
func withLockAt(dir, lockPath string, fn func() error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ensure registry dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
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
