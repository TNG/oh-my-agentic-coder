// Package sandboxrun builds and launches the platform kernel sandbox.
//
// This file provides ProtectedPathSet — a read-only snapshot of the
// resolved protected paths that the facade (running in the parent
// process) consults to answer "is this path protected?" queries from
// the agent. The grants themselves are resolved inside the child
// (`omac sandbox run`), but the protected set is cheap to re-derive
// from the profile file: it needs only path expansion (no existence
// checks, no walks), so the parent duplicates only the light part.
package sandboxrun

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// ProtectedPathSet answers whether a given absolute path lies under any
// protected path. It is the facade-side counterpart to the Grants
// computed by ResolveGrants. Launch-derived entries are immutable; the
// mid-session watch appends dynamic entries via Add. Safe for
// concurrent use.
type ProtectedPathSet struct {
	// entries are the expanded protected paths (absolute).
	entries []string
	// rule tags which layer each entry came from, parallel to entries.
	// "baseline" for the platform default set, "profile" for user deny
	// entries. This is a coarse tag — the goal is "tell the agent which
	// knob to turn," not a full audit trail.
	rules []string
	// dyn* hold the entries Add appends while the session runs:
	// protected-pattern matches created after launch. Guarded by dynMu.
	dynMu      sync.Mutex
	dynEntries []string
	dynRules   []string
}

// NewProtectedPathSet derives the protected set from a resolved profile.
// It re-derives the baseline + user deny paths the same way
// ResolveGrants does, but skips existence filtering and glob walks
// (those need the workdir and granted trees; the facade doesn't have
// them). Baseline and explicit path-form denies are enough to answer
// "is this the kind of path the sandbox protects?" for the agent.
func NewProtectedPathSet(p *sandboxprofile.Profile, workdir string) *ProtectedPathSet {
	base := sandboxprofile.PlatformBaseline()
	protected := sandboxprofile.EffectiveProtectedPaths(base, p.Filesystem.OverrideDeny)

	set := &ProtectedPathSet{}
	for _, pp := range protected {
		if exp, err := sandboxprofile.ExpandPath(pp); err == nil {
			set.entries = append(set.entries, exp)
			set.rules = append(set.rules, "baseline")
		}
	}
	// The omac config directories are never overridable (see
	// NonOverridableProtectedPaths), so the facade must report them even when
	// the profile lists them in override_deny.
	for _, exp := range sandboxprofile.NonOverridableProtectedPaths(config.LocalConfigDir(workdir)) {
		set.entries = append(set.entries, exp)
		set.rules = append(set.rules, "omac")
	}
	// User path-form denies (globs are skipped — they need a walk). Shared
	// with ResolveGrants via pathFormDenies so both agree on which paths
	// count as explicitly denied.
	for _, exp := range pathFormDenies(p.Filesystem.Deny, nil) {
		set.entries = append(set.entries, exp)
		set.rules = append(set.rules, "profile")
	}
	return set
}

// UnrestrictedProtectedPathSet returns the set that survives learn mode
// (`omac serve --learn`): the non-overridable omac config directories. Learn
// mode lifts the profile's protected paths and grants "/", but the config
// directories stay masked so a session cannot plant a .omac/ a later launch
// would trust. Reporting the profile's static set during a learn session would
// tell the agent that a genuinely missing file was blocked by the sandbox,
// which is the confusion GET /sandbox/denied exists to remove.
func UnrestrictedProtectedPathSet(workdir string) *ProtectedPathSet {
	set := &ProtectedPathSet{}
	for _, exp := range sandboxprofile.NonOverridableProtectedPaths(config.LocalConfigDir(workdir)) {
		set.entries = append(set.entries, exp)
		set.rules = append(set.rules, "omac")
	}
	return set
}

// Add records a protected-pattern path discovered while the session runs
// (the mid-session watch). The kernel mask was fixed at launch, so the
// path is NOT blocked — the rule tag (RuleMidSession) is what makes the
// facade report it honestly. Nil-safe.
func (s *ProtectedPathSet) Add(absPath, rule string) {
	if s == nil {
		return
	}
	s.dynMu.Lock()
	defer s.dynMu.Unlock()
	s.dynEntries = append(s.dynEntries, filepath.Clean(absPath))
	s.dynRules = append(s.dynRules, rule)
}

// IsProtected reports whether absPath lies under (or equals) any
// protected entry. Returns the rule tag of the first match.
func (s *ProtectedPathSet) IsProtected(absPath string) (rule string, ok bool) {
	if s == nil {
		return "", false
	}
	// Normalize: clean the path but don't follow symlinks (the agent
	// queries with the path it tried; we match lexically).
	absPath = filepath.Clean(absPath)
	for i, entry := range s.entries {
		entry = filepath.Clean(entry)
		if absPath == entry || strings.HasPrefix(absPath, entry+string(filepath.Separator)) {
			return s.rules[i], true
		}
	}
	s.dynMu.Lock()
	defer s.dynMu.Unlock()
	for i, entry := range s.dynEntries {
		entry = filepath.Clean(entry)
		if absPath == entry || strings.HasPrefix(absPath, entry+string(filepath.Separator)) {
			return s.dynRules[i], true
		}
	}
	return "", false
}
