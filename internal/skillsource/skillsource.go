// Package skillsource resolves skill source directories. omac looks
// for a skill named X across two layers — workdir-local and
// user-global. Within each layer, omac honors two parallel naming
// conventions: the agentskills.io-aligned `.agents/skills` location
// and the legacy `.opencode/skills` location. Lookup order is:
//
//	workdir-local:
//	  1. <workdir>/.agents/skills/X
//	  2. <workdir>/.opencode/skills/X
//
//	user-global (only roots that exist on disk are scanned):
//	  3. $XDG_CONFIG_HOME/agents/skills/X     (if XDG_CONFIG_HOME set)
//	  4. $XDG_CONFIG_HOME/opencode/skills/X   (if XDG_CONFIG_HOME set)
//	  5. $HOME/.config/agents/skills/X
//	  6. $HOME/.config/opencode/skills/X
//	  7. $HOME/.agents/skills/X               (legacy flat, agents)
//	  8. $HOME/.opencode/skills/X             (legacy flat, opencode)
//
// `.agents/skills` ranks above `.opencode/skills` in every layer so a
// project can override an existing `.opencode/skills` entry by
// dropping a sibling under `.agents/skills`. Workdir-local always
// wins over user-global on name collision.
//
// What omac considers a "skill" is the same across every root: a
// directory containing an `omac.yaml` at its top level. A directory
// with only a `SKILL.md` is a valid agentskills.io skill but does
// not have an omac sidecar contract, so omac ignores it (no
// registration, no spawning).
//
// Registration data follows the source layer. A skill resolved from
// a workdir-local source records its registry entry and config in
// that workdir (.opencode/sidecar.json, .opencode/skill-config.yaml).
// A skill resolved from a user-global source records its registry
// entry and config once, globally (~/.config/omac/sidecar.json,
// ~/.config/omac/skill-config.yaml — XDG_CONFIG_HOME honored), so a
// single `omac register <name>` makes it available in every workdir.
// Keychain secrets are keyed by skill name and are therefore already
// global. When both layers hold state for the same skill name, the
// workdir layer wins.
package skillsource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

// Source describes one location omac looks in for skill source dirs.
type Source struct {
	// Root is the absolute path of the directory that holds skill
	// subdirectories. Skill X lives at filepath.Join(Root, X).
	Root string
	// Kind is a short label for diagnostics ("workdir" or "user-global").
	Kind string
}

// Sources returns the candidate roots in priority order. Workdir-local
// roots come first; subsequent elements are user-global candidates
// that actually exist on disk.
//
// Workdir roots are always included (even if they don't exist yet)
// because that's where new skills will be created. User-global roots
// are only included when present, since dangling references would
// just produce noisy "stat: no such file" errors at scan time.
//
// Within each layer, `.agents/skills` (the agentskills.io-aligned
// location) ranks above `.opencode/skills` (the legacy location), so
// a project can override an existing `.opencode/skills` entry by
// dropping a sibling under `.agents/skills`.
func Sources(workdir string) []Source {
	out := []Source{
		{Root: filepath.Join(workdir, ".agents", "skills"), Kind: "workdir"},
		{Root: filepath.Join(workdir, ".opencode", "skills"), Kind: "workdir"},
	}
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
// order. Sources() picks every candidate that exists on disk; missing
// ones are silently skipped.
//
// Within each base location (XDG/explicit, XDG/default, legacy flat)
// we surface BOTH the agents-skills root and the opencode-skills
// root, with agents ranked first to match the in-workdir ordering.
//
// The order is:
//
//	if $XDG_CONFIG_HOME set:
//	  1. $XDG_CONFIG_HOME/agents/skills
//	  2. $XDG_CONFIG_HOME/opencode/skills
//	XDG default (always tried):
//	  3. $HOME/.config/agents/skills
//	  4. $HOME/.config/opencode/skills
//	legacy flat layout:
//	  5. $HOME/.agents/skills
//	  6. $HOME/.opencode/skills
//
// On a fresh macOS/Linux box where $XDG_CONFIG_HOME is unset (or set
// to its default of $HOME/.config), the XDG-style and default paths
// collapse; dedupe() drops the duplicates so each filesystem path
// appears at most once.
func userGlobalRoots() []string {
	var out []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		out = append(out, filepath.Join(xdg, "agents", "skills"))
		out = append(out, filepath.Join(xdg, "opencode", "skills"))
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return out
	}
	out = append(out, filepath.Join(home, ".config", "agents", "skills"))
	out = append(out, filepath.Join(home, ".config", "opencode", "skills"))
	out = append(out, filepath.Join(home, ".agents", "skills"))
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
		metaPath := filepath.Join(candidate, config.MetaFileName)
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
// considered a skill if and only if it contains a omac.yaml at its
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
			metaPath := filepath.Join(s.Root, ent.Name(), config.MetaFileName)
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
