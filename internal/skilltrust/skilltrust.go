// Package skilltrust records host-only approvals that authorize omac to
// spawn a skill sidecar.
//
// A skill sidecar runs as an ordinary, UNSANDBOXED host process (see
// internal/supervisor): it has the host's network and can read the host
// filesystem, because legitimate skills (e.g. the marketplace) need that
// reach. The sandboxed coding agent must never gain it. But the agent
// CAN write the workdir — including the skill source dirs
// (.claude/.opencode/.agents/skills/*) and the workdir registry
// (.opencode/sidecar.json). So if the decision to spawn a skill were
// anchored only in the workdir, a confined agent could author a skill,
// forge its registration, trigger a spawn, and thereby execute code
// outside the sandbox.
//
// This package anchors that decision OUTSIDE the agent-writable
// filesystem. The approvals file lives under registry.GlobalDir()
// (~/.config/omac, honoring $XDG_CONFIG_HOME) — a directory the default
// sandbox profile never mounts into the sandbox, so the confined agent
// cannot create or edit it. The kernel sandbox, not omac's own trust in
// a file, is what makes an approval unforgeable.
//
// An approval is keyed by (skill name, bundle hash). The bundle hash
// covers every meaningful file in the skill directory (see
// config.BundleHash), so editing a skill's sidecar code changes its hash
// and silently invalidates the approval — closing the "edit a trusted
// skill's code" vector as well as the "author a new skill" one.
//
// Only actors on the host side of the sandbox boundary can approve: a
// human running `omac register` in a real terminal, or the marketplace
// sidecar (itself a host process) after installing a skill. Running
// `omac register` from inside the sandbox fails to write here (the path
// is not mounted), so the skill stays unapproved and will not spawn.
package skilltrust

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
)

// SchemaVersion is the current on-disk format version.
const SchemaVersion = 1

// fileName is the approvals file's basename inside registry.GlobalDir().
const fileName = "approvals.json"

// Approval authorizes spawning a specific skill build. Name plus
// BundleHash together identify the exact approved content; SkillDir and
// ApprovedAt are informational (diagnostics / audit).
type Approval struct {
	Name       string    `json:"name"`
	BundleHash string    `json:"bundle_hash"`
	SkillDir   string    `json:"skill_dir,omitempty"`
	ApprovedAt time.Time `json:"approved_at"`
}

// Store is the root object of approvals.json.
type Store struct {
	Version  int        `json:"version"`
	Approved []Approval `json:"approved"`
}

// ErrNoGlobalDir is returned when no host-only config directory can be
// resolved (neither $XDG_CONFIG_HOME nor $HOME is set), so approvals
// cannot be anchored. Callers treat this as "cannot approve / nothing is
// approved" and fail closed.
var ErrNoGlobalDir = errors.New("skilltrust: no host-only config directory available (set $HOME or $XDG_CONFIG_HOME)")

// dir returns the host-only approvals directory (~/.config/omac), shared
// with the user-global registry so both live in the same non-mounted
// location.
func dir() string { return registry.GlobalDir() }

// Path returns the approvals file path, or "" when no host-only
// directory can be resolved.
func Path() string {
	d := dir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, fileName)
}

// lockPath returns the flock path guarding the approvals file.
func lockPath() string {
	d := dir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, fileName+".lock")
}

// Exists reports whether the approvals file has been created yet. It is
// the signal used by one-time migration (see caller): a missing file
// means this host has not yet been migrated to approval-gated spawning.
func Exists() bool {
	p := Path()
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// Load reads the approvals store. A missing file (or unresolvable
// directory) returns an empty store, so callers can always check
// membership; an empty store approves nothing (fail closed).
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
		return nil, fmt.Errorf("read approvals: %w", err)
	}
	var s Store
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse approvals: %w", err)
	}
	if s.Version == 0 {
		s.Version = SchemaVersion
	}
	return &s, nil
}

// IsApproved reports whether (name, bundleHash) has a recorded approval.
// A read error fails closed (returns false, err) so a caller that
// ignores the error still denies the spawn.
func IsApproved(name, bundleHash string) (bool, error) {
	s, err := Load()
	if err != nil {
		return false, err
	}
	for _, a := range s.Approved {
		if a.Name == name && a.BundleHash == bundleHash {
			return true, nil
		}
	}
	return false, nil
}

// Approve records an approval for (name, bundleHash). Approvals are
// additive and keyed by (name, bundle hash): the same skill name may hold
// several approved hashes at once, because a name can be registered under
// more than one harness (each with its own content) and workdir-local
// skills are per-project — so approving one must never invalidate another.
// Re-approving an identical (name, hash) is idempotent.
//
// It must be called from the host side of the sandbox boundary; inside the
// sandbox the write fails because the directory is not mounted, leaving the
// skill unapproved.
func Approve(name, bundleHash, skillDir string) error {
	if dir() == "" {
		return ErrNoGlobalDir
	}
	return withLock(func() error {
		s, err := Load()
		if err != nil {
			return err
		}
		for _, a := range s.Approved {
			if a.Name == name && a.BundleHash == bundleHash {
				return nil // already approved; nothing to change
			}
		}
		s.Approved = append(s.Approved, Approval{
			Name:       name,
			BundleHash: bundleHash,
			SkillDir:   skillDir,
			ApprovedAt: time.Now().UTC(),
		})
		return save(s)
	})
}

// Revoke removes the approval for exactly (name, bundleHash). Returns true
// when something was removed. Used by `omac deregister`, which passes the
// deregistered entry's own hash so a copy of the same skill still
// registered under another harness or workdir keeps its approval.
func Revoke(name, bundleHash string) (bool, error) {
	if dir() == "" {
		return false, ErrNoGlobalDir
	}
	removed := false
	err := withLock(func() error {
		s, err := Load()
		if err != nil {
			return err
		}
		out := s.Approved[:0:0]
		for _, a := range s.Approved {
			if a.Name == name && a.BundleHash == bundleHash {
				removed = true
				continue
			}
			out = append(out, a)
		}
		s.Approved = out
		return save(s)
	})
	return removed, err
}

// EnsureInitialized creates an empty approvals store if none exists yet,
// so the "first upgraded run" is a single event: once this has run, Exists
// reports true even when nothing was approved. Without it a host that has
// never registered a skill would keep looking like a first upgrade on
// every launch, re-opening the one-time grandfathering window. No-op when
// the store already exists or no host-only dir is resolvable.
func EnsureInitialized() error {
	if dir() == "" || Exists() {
		return nil
	}
	return withLock(func() error {
		if Exists() {
			return nil
		}
		return save(&Store{Version: SchemaVersion})
	})
}

// save atomically writes s (write-temp + rename). The caller holds the
// lock (see withLock).
func save(s *Store) error {
	d := dir()
	if d == "" {
		return ErrNoGlobalDir
	}
	if s.Version == 0 {
		s.Version = SchemaVersion
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return fmt.Errorf("ensure approvals dir: %w", err)
	}
	sort.SliceStable(s.Approved, func(i, j int) bool {
		if s.Approved[i].Name != s.Approved[j].Name {
			return s.Approved[i].Name < s.Approved[j].Name
		}
		return s.Approved[i].BundleHash < s.Approved[j].BundleHash
	})
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal approvals: %w", err)
	}
	tmp, err := os.CreateTemp(d, fileName+".tmp-*")
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
	if err := os.Rename(tmpPath, Path()); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename approvals: %w", err)
	}
	return nil
}

// withLock acquires an exclusive flock for the duration of fn.
func withLock(fn func() error) error {
	d := dir()
	if d == "" {
		return ErrNoGlobalDir
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return fmt.Errorf("ensure approvals dir: %w", err)
	}
	f, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
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
