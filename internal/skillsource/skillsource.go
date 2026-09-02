// Package skillsource resolves skill source directories. Discovery is
// HARNESS-SCOPED: the active harness (config.Harness) determines which skills
// roots omac scans. Each harness owns a skills base — OpenCode owns "opencode"
// (.opencode/skills), Claude Code owns "claude" (.claude/skills) — matching
// where that harness's own loader reads SKILL.md. A shared neutral base,
// "agents" (.agents/skills), is in scope for every harness. omac NEVER scans
// another harness's base, so a skill that belongs to the inactive harness is
// invisible to the active run.
//
// For the OpenCode harness, lookup order is:
//
//	workdir-local:
//	  1. <workdir>/.opencode/skills/X   (own base)
//	  2. <workdir>/.agents/skills/X     (shared base)
//
//	user-global (only roots that exist on disk are scanned):
//	  3. $XDG_CONFIG_HOME/opencode/skills/X, $XDG_CONFIG_HOME/agents/skills/X
//	  4. $HOME/.config/opencode/skills/X,    $HOME/.config/agents/skills/X
//	  5. $HOME/.opencode/skills/X,           $HOME/.agents/skills/X
//
// For the Claude Code harness, substitute "claude" for "opencode" above; the
// "opencode" roots are NOT scanned, and vice versa.
//
// The harness's own base ranks above the shared `agents` base in every layer,
// so a harness-specific skill overrides a neutral one of the same name.
// Workdir-local always wins over user-global on name collision.
//
// What omac considers a "skill" is the same across every root: a directory
// containing an `omac.yaml` at its top level. A directory with only a
// `SKILL.md` is a valid agentskills.io skill but does not have an omac sidecar
// contract, so omac ignores it (no registration, no spawning).
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
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// Source describes one location omac looks in for skill source dirs.
type Source struct {
	// Root is the absolute path of the directory that holds skill
	// subdirectories. Skill X lives at filepath.Join(Root, X).
	Root string
	// Kind is a short label for diagnostics ("workdir" or "user-global").
	Kind string
}

// Sources returns the candidate roots in priority order for the active
// harness. Discovery is HARNESS-SCOPED: only the harness's own skills base
// (e.g. "opencode" → .opencode/skills, "claude" → .claude/skills) plus the
// shared neutral base ("agents" → .agents/skills) are scanned. Another
// harness's base is never scanned, so a skill belonging to the inactive
// harness is invisible.
//
// Workdir-local roots come first; subsequent elements are user-global
// candidates that actually exist on disk. Within each layer the harness's own
// base ranks above the shared `agents` base, so a harness-specific skill
// overrides a neutral one of the same name. Workdir-local always wins over
// user-global.
//
// Workdir roots are always included (even if they don't exist yet) because
// that's where new skills will be created. User-global roots are only included
// when present, since dangling references would just produce noisy "stat: no
// such file" errors at scan time.
func Sources(workdir string, harness config.Harness) []Source {
	bases := harness.InScopeSkillsBases() // own base first, then shared

	var out []Source
	// Workdir layer, in base priority order.
	for _, base := range bases {
		out = append(out, Source{
			Root: filepath.Join(workdir, "."+base, "skills"),
			Kind: "workdir",
		})
	}
	// User-global layer, in base priority order; only existing dirs.
	for _, root := range userGlobalRoots(bases) {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			out = append(out, Source{Root: root, Kind: "user-global"})
		}
	}
	return out
}

// userGlobalRoots returns user-config-dir candidates for the given skills
// bases, in priority order. Sources() picks every candidate that exists on
// disk; missing ones are silently skipped.
//
// For each base location (XDG/explicit, XDG/default, legacy flat) we surface
// the roots for the in-scope bases, base priority preserved (own-harness base
// before the shared "agents" base). For example, with bases ["opencode",
// "agents"] the order is:
//
//	if $XDG_CONFIG_HOME set:
//	  $XDG_CONFIG_HOME/opencode/skills, $XDG_CONFIG_HOME/agents/skills
//	XDG default (always tried):
//	  $HOME/.config/opencode/skills,    $HOME/.config/agents/skills
//	legacy flat layout:
//	  $HOME/.opencode/skills,           $HOME/.agents/skills
//
// dedupe() drops duplicates that arise when $XDG_CONFIG_HOME == $HOME/.config.
func userGlobalRoots(bases []string) []string {
	var out []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		for _, base := range bases {
			out = append(out, filepath.Join(xdg, base, "skills"))
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return dedupe(out)
	}
	for _, base := range bases {
		out = append(out, filepath.Join(home, ".config", base, "skills"))
	}
	for _, base := range bases {
		out = append(out, filepath.Join(home, "."+base, "skills"))
	}
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
// highest-priority in-scope source (for the active harness) that has it. The
// returned Source identifies which layer matched (handy for diagnostic
// messages like "found in user-global skills"). os.ErrNotExist is returned
// when no in-scope layer has the skill, so callers can errors.Is against it.
func Resolve(workdir string, harness config.Harness, name string) (absDir string, src Source, err error) {
	for _, s := range Sources(workdir, harness) {
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

// Candidate is one in-scope source directory that contains a skill of a
// given name. Unlike Resolve (which returns only the winner), Candidates
// returns every in-scope match so the caller can detect ambiguity.
type Candidate struct {
	Name    string
	Dir     string // absolute source directory
	Kind    string // "workdir" | "user-global"
	Harness string // canonical name of the harness whose scope this came from
}

// Candidates returns every in-scope source for `name` under the active
// harness, in priority order (workdir before global, own-base before shared).
// An empty slice means not found. More than one element means the name is
// ambiguous within this harness's scope (e.g. workdir + global) and the caller
// should ask the user to disambiguate.
func Candidates(workdir string, harness config.Harness, name string) ([]Candidate, error) {
	var out []Candidate
	for _, s := range Sources(workdir, harness) {
		candidate := filepath.Join(s.Root, name)
		metaPath := filepath.Join(candidate, config.MetaFileName)
		if _, err := os.Stat(metaPath); err == nil {
			out = append(out, Candidate{
				Name:    name,
				Dir:     candidate,
				Kind:    s.Kind,
				Harness: harness.Name,
			})
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("skillsource: stat %s: %w", metaPath, err)
		}
	}
	return out, nil
}

// CandidatesAllHarnesses returns every in-scope source for `name` across ALL
// known harnesses, de-duplicated by absolute directory (the shared `.agents`
// roots are in every harness's scope, so a shared skill would otherwise appear
// once per harness — it is collapsed to a single Candidate whose Harness is
// SharedHarnessLabel). This lets `omac register` detect that a name exists
// under more than one *harness* (a harness-level ambiguity) and ask the user
// to pick with --harness. Order: by harness (registry order), then source
// priority.
func CandidatesAllHarnesses(workdir string, name string) ([]Candidate, error) {
	var out []Candidate
	seenDir := make(map[string]int) // abs dir -> index in out
	for _, h := range config.AllHarnesses() {
		cs, err := Candidates(workdir, h, name)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			if idx, ok := seenDir[c.Dir]; ok {
				// Same physical dir reachable from multiple harnesses =>
				// it's a shared (.agents) skill. Mark it as shared so the
				// caller does not treat it as harness-ambiguous.
				out[idx].Harness = SharedHarnessLabel
				continue
			}
			seenDir[c.Dir] = len(out)
			out = append(out, c)
		}
	}
	return out, nil
}

// SharedHarnessLabel marks a Candidate that lives in the shared `.agents`
// skills root and is therefore in scope for every harness (not specific to
// one). Used in disambiguation output.
const SharedHarnessLabel = "shared"

// DirInHarnessScope reports whether an (absolute or relative) skill directory
// belongs to the active harness's scope, based on the "<base>/skills" segment
// in its path. A directory whose parent-of-parent segment is the harness's own
// base or the shared `agents` base is in scope; a directory that sits under
// another harness's base (e.g. ".../opencode/skills/X" while the active
// harness is claude-code) is NOT.
//
// This is used for user-global registry entries, which store absolute SkillDir
// paths and therefore bypass directory-scanning discovery. A path that matches
// no known skills base at all (e.g. a hand-rolled location) is treated as
// in scope, so omac does not silently drop a deliberately custom registration.
func DirInHarnessScope(dir string, harness config.Harness) bool {
	base, ok := skillsBaseOfDir(dir)
	if !ok {
		// Not under any recognizable "<base>/skills" path; don't exclude it.
		return true
	}
	return harness.SkillsBaseInScope(base)
}

// skillsBaseOfDir extracts the "<base>" from a path ending in
// ".../<base>/skills/<name>" or ".../.<base>/skills/<name>" (leading dot for
// workdir-relative dirs like ".opencode/skills/X"). Returns the base with any
// leading dot stripped and ok=false if the path has no "skills" parent segment
// recognizable as one of the known bases.
func skillsBaseOfDir(dir string) (string, bool) {
	clean := filepath.Clean(dir)
	// Walk up: <name> / skills / <base>
	skillsParent := filepath.Dir(clean) // .../<base>/skills
	if filepath.Base(skillsParent) != "skills" {
		return "", false
	}
	baseDir := filepath.Dir(skillsParent) // .../<base>
	base := strings.TrimPrefix(filepath.Base(baseDir), ".")
	// Only treat it as a "skills base" if it is a known one (a harness base or
	// the shared base); otherwise we can't classify it and the caller defaults
	// to in-scope.
	if base == config.SharedSkillsBase {
		return base, true
	}
	for _, hb := range config.AllHarnessSkillsBases() {
		if base == hb {
			return base, true
		}
	}
	return "", false
}

// Entry is one skill discovered by Discover. It pairs the skill name
// with its absolute source directory and the layer it came from.
type Entry struct {
	Name string
	Dir  string // absolute
	Kind string // "workdir" | "user-global"
}

// Discover returns every in-scope skill (for the active harness) found across
// every source, with duplicates resolved by precedence (workdir wins, own-base
// before shared). A directory is considered a skill if and only if it contains
// a omac.yaml at its top level. Returns the entries unsorted; callers that want
// deterministic output should sort by Name.
//
// Errors from individual readdir calls bubble up unless they are "directory
// does not exist", which we treat as "no skills here".
func Discover(workdir string, harness config.Harness) ([]Entry, error) {
	seen := make(map[string]struct{})
	var out []Entry
	for _, s := range Sources(workdir, harness) {
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
