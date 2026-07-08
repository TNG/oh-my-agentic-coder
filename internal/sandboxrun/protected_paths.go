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

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// ProtectedPathSet answers whether a given absolute path lies under any
// protected path. It is the facade-side counterpart to the Grants
// computed by ResolveGrants. Implementations are read-only after
// construction; safe for concurrent use.
type ProtectedPathSet struct {
	// entries are the expanded protected paths (absolute).
	entries []string
	// rule tags which layer each entry came from, parallel to entries.
	// "baseline" for the platform default set, "profile" for user deny
	// entries. This is a coarse tag — the goal is "tell the agent which
	// knob to turn," not a full audit trail.
	rules []string
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
	// User path-form denies (globs are skipped — they need a walk).
	for _, d := range p.Filesystem.Deny {
		if sandboxprofile.IsBasenameGlob(d) {
			continue
		}
		exp, err := sandboxprofile.ExpandPath(d)
		if err != nil {
			continue
		}
		set.entries = append(set.entries, exp)
		set.rules = append(set.rules, "profile")
	}
	return set
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
	return "", false
}
