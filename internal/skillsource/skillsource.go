// Package skillsource resolves skill source directories. omac looks
// for a skill named X in two layers, in this order:
//
//  1. <workdir>/.opencode/skills/X        — workdir-local
//  2. <user-config>/opencode/skills/X     — user-global
//
// Workdir wins on collision, so a workdir-local skill can override a
// user-global one with the same name (handy for project-specific
// forks).
//
// Where does <user-config> resolve to?
//
//   - If $XDG_CONFIG_HOME is set and non-empty, it is honored:
//     $XDG_CONFIG_HOME/opencode/skills.
//   - Otherwise we fall back to $HOME/.config/opencode/skills.
//   - As a final compatibility fallback (for users who set up their
//     dotfiles before omac existed and still have a flat layout),
//     $HOME/.opencode/skills is consulted if the XDG-style path
//     doesn't exist.
//
// Registration data (sidecar.json, skill-config.yaml, keychain
// entries) always lives in the workdir regardless of where the
// skill source came from. Each project explicitly opts in to a
// user-global skill by running `omac register <name>` in that
// project's workdir.
package skillsource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Source describes one location omac looks in for skill source dirs.
type Source struct {
	// Root is the absolute path of the directory that holds skill
	// subdirectories. Skill X lives at filepath.Join(Root, X).
	Root string
	// Kind is a short label for diagnostics ("workdir" or "user-global").
	Kind string
}

// Sources returns the candidate roots in priority order. The first
// element is always the workdir; subsequent elements are user-global
// candidates that actually exist on disk.
//
// The workdir root is always included (even if it doesn't exist yet)
// because that's where new skills will be created. User-global roots
// are only included when present, since dangling references would
// just produce noisy "stat: no such file" errors at scan time.
func Sources(workdir string) []Source {
	out := []Source{{
		Root: filepath.Join(workdir, ".opencode", "skills"),
		Kind: "workdir",
	}}
	for _, root := range userGlobalRoots() {
		// Don't bother including a root that isn't a directory; the
		// scanner would return ENOENT every time. ReadDir on a
		// non-existent dir is the cheaper check than Stat-then-ReadDir.
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			out = append(out, Source{Root: root, Kind: "user-global"})
		}
	}
	return out
}

// userGlobalRoots returns user-config-dir candidates in priority
// order. Only the first hit on disk is consumed, but we surface all
// candidates so Sources() can pick the existing one.
//
// The order is:
//
//  1. $XDG_CONFIG_HOME/opencode/skills  (only if XDG_CONFIG_HOME set)
//  2. $HOME/.config/opencode/skills      (XDG default on macOS+Linux)
//  3. $HOME/.opencode/skills             (legacy flat layout)
//
// 1 and 2 are usually the same path on a fresh macOS or Linux box
// (XDG_CONFIG_HOME defaults to ~/.config); they only diverge when
// the user has explicitly set XDG_CONFIG_HOME.
func userGlobalRoots() []string {
	var out []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		out = append(out, filepath.Join(xdg, "opencode", "skills"))
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return out
	}
	out = append(out, filepath.Join(home, ".config", "opencode", "skills"))
	out = append(out, filepath.Join(home, ".opencode", "skills"))
	return dedupe(out)
}

// dedupe returns paths in original order with duplicates removed.
// Useful when XDG_CONFIG_HOME points at $HOME/.config, which makes
// items 1 and 2 above identical.
func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// Resolve returns the absolute directory of skill `name` from the
// highest-priority source that has it. The returned Source identifies
// which layer matched (handy for diagnostic messages like "found in
// user-global skills"). os.ErrNotExist is returned when no layer has
// the skill, so callers can errors.Is against it.
func Resolve(workdir, name string) (absDir string, src Source, err error) {
	for _, s := range Sources(workdir) {
		candidate := filepath.Join(s.Root, name)
		metaPath := filepath.Join(candidate, "meta.yaml")
		if _, err := os.Stat(metaPath); err == nil {
			return candidate, s, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			// Permission error on a directory we tried to descend
			// into — surface it; silently swallowing it would hide
			// real bugs (e.g. a 700 dir owned by root).
			return "", Source{}, fmt.Errorf("skillsource: stat %s: %w", metaPath, err)
		}
	}
	return "", Source{}, fmt.Errorf("skillsource: %q not found in any source: %w", name, os.ErrNotExist)
}

// Entry is one skill discovered by Discover. It pairs the skill name
// with its absolute source directory and the layer it came from.
type Entry struct {
	Name string
	Dir  string // absolute
	Kind string // "workdir" | "user-global"
}

// Discover returns every skill found across every source, with
// duplicates resolved by precedence (workdir wins). A directory is
// considered a skill if and only if it contains a meta.yaml at its
// top level. Returns the entries unsorted; callers that want
// deterministic output should sort by Name.
//
// Errors from individual readdir calls bubble up unless they are
// "directory does not exist", which we treat as "no skills here".
func Discover(workdir string) ([]Entry, error) {
	seen := make(map[string]struct{})
	var out []Entry
	for _, s := range Sources(workdir) {
		entries, err := os.ReadDir(s.Root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("skillsource: read %s: %w", s.Root, err)
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			if _, dup := seen[ent.Name()]; dup {
				continue
			}
			metaPath := filepath.Join(s.Root, ent.Name(), "meta.yaml")
			if _, err := os.Stat(metaPath); err != nil {
				continue
			}
			seen[ent.Name()] = struct{}{}
			out = append(out, Entry{
				Name: ent.Name(),
				Dir:  filepath.Join(s.Root, ent.Name()),
				Kind: s.Kind,
			})
		}
	}
	return out, nil
}
