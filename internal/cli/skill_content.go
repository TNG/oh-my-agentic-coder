package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/skillsource"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
)

// The harness's own loader reads SKILL.md straight from the skill dirs and
// hands it to the model as trusted instructions. Those dirs are writable from
// inside the sandbox, so a session could rewrite the instructions every later
// session receives (#393). The approval gate in skill_approval.go only decides
// which sidecar code runs; this file decides which skill instructions the
// sandboxed harness may see. A skill dir that does not match a host approval is
// hidden from the sandbox for the whole session.

// skillDocName is the file harnesses load as a skill's instructions.
const skillDocName = "SKILL.md"

// injectUntrustedSkillDenies hides every omac skill dir whose content is not
// anchored in the host-only approval store, by splicing a --deny for it into
// the sandbox argv. Each hidden skill gets one line on w naming the reason and
// the fix. Only omac's native sandbox understands --deny; on another backend
// the skill stays visible and the line says so.
//
// A skill root that cannot be listed is hidden whole rather than skipped: the
// sandbox can make a root unreadable (e.g. replace .claude/skills with a file),
// and that must not switch the check off for it or for the other roots.
func injectUntrustedSkillDenies(argv []string, w io.Writer, prefix string, prof config.SandboxProfile, workdirs ...string) []string {
	dirs, unreadable := omacSkillDirs(workdirs...)
	_, native := prof.PolicyRef()
	hide := func(path string, reason error) {
		if !native {
			fmt.Fprintf(w, "%s: %v. NOT hidden: only the built-in sandbox can hide it\n", prefix, reason)
			return
		}
		fmt.Fprintf(w, "%s: %v. Hidden from this session\n", prefix, reason)
		argv = injectSandboxFlag(argv, "--deny", path)
	}
	for _, r := range unreadable {
		hide(r.root, fmt.Errorf("cannot check the skills in %s: %w", r.root, r.err))
	}
	for _, sk := range dirs {
		if refusal := skillContentRefusal(sk.Name, sk.Dir); refusal != nil {
			hide(sk.Dir, refusal)
		}
	}
	return argv
}

// unreadableSkillRoot is a skill root that exists but could not be listed.
type unreadableSkillRoot struct {
	root string
	err  error
}

// omacSkillDirs lists every omac skill dir under the given workdirs and the
// user-global skill roots that a harness loader might read SKILL.md from. A
// dir counts as an omac skill when it has an omac.yaml, or when the host
// approval store knows its name: deleting omac.yaml must not turn an approved
// skill into an unchecked plain one. Unlike skillsource.Discover it keeps a
// dir even when a higher-priority dir has the same name, follows symlinked
// skill dirs, and covers every harness's base: a shadowed copy is still
// loaded, and OpenCode also reads .claude/skills.
func omacSkillDirs(workdirs ...string) ([]skillsource.Entry, []unreadableSkillRoot) {
	var sources []skillsource.Source
	for _, wd := range workdirs {
		for _, h := range config.AllHarnesses() {
			sources = append(sources, skillsource.Sources(wd, h)...)
		}
	}
	seen := map[string]bool{}
	var out []skillsource.Entry
	var unreadable []unreadableSkillRoot
	for _, src := range sources {
		if seen[src.Root] {
			continue
		}
		seen[src.Root] = true
		ents, err := os.ReadDir(src.Root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			unreadable = append(unreadable, unreadableSkillRoot{root: src.Root, err: err})
			continue
		}
		for _, ent := range ents {
			dir := filepath.Join(src.Root, ent.Name())
			if seen[dir] {
				continue
			}
			// os.Stat follows a symlinked skill dir.
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				continue
			}
			_, metaErr := os.Lstat(filepath.Join(dir, config.MetaFileName))
			if errors.Is(metaErr, os.ErrNotExist) {
				// A read error counts as known (fail closed); the
				// refusal then reports it.
				if known, _ := skilltrust.HasName(ent.Name()); !known {
					continue
				}
			}
			seen[dir] = true
			out = append(out, skillsource.Entry{Name: ent.Name(), Dir: dir, Kind: src.Kind})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, unreadable
}

// skillContentRefusal reports why the instructions in skillDir must not reach
// the sandboxed harness, or nil when they match a host approval. It never
// consults the workdir registry (.opencode/sidecar.json): the sandbox can write
// that file, so a hash recorded there proves nothing.
func skillContentRefusal(name, skillDir string) error {
	if fi, err := os.Lstat(filepath.Join(skillDir, config.MetaFileName)); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("skill %q: %s is missing or not a regular file, so the skill cannot be checked and its SKILL.md instructions are untrusted; restore it, or remove the skill folder", name, filepath.Join(skillDir, config.MetaFileName))
	}
	rel, err := unhashedSkillDoc(skillDir)
	if err != nil {
		return fmt.Errorf("skill %q: cannot check %s: %w", name, skillDir, err)
	}
	if rel != "" {
		return fmt.Errorf("skill %q: %s is not covered by its approval (symlinks, folders linked from outside the skill, and files under dirs like build/ or node_modules/ are not hashed); remove it", name, filepath.Join(skillDir, rel))
	}
	h, err := config.BundleHash(skillDir)
	if err != nil {
		return fmt.Errorf("skill %q: cannot hash %s: %w", name, skillDir, err)
	}
	approved, err := skilltrust.IsApproved(name, h)
	if err != nil {
		return fmt.Errorf("skill %q: cannot read the host approval store: %w", name, err)
	}
	if !approved {
		return fmt.Errorf("skill %q: its files do not match an approved version, so its SKILL.md instructions are untrusted; review %s, then run `omac register %s --force` from a host terminal", name, skillDir, name)
	}
	return nil
}

// unhashedSkillDoc returns the path (relative to skillDir) of the first thing
// a harness could load as a SKILL.md although config.BundleHash does not cover
// it, or "" when there is none. Harness loaders search a skill dir
// recursively and follow symlinks, while the hash skips symlinks and
// excluded dirs. Without this check an approved skill could carry planted
// instructions, e.g. build/SKILL.md, and still match its approval.
//
// A symlinked dir pointing inside the skill is not reported: the walk visits
// its target where it really is. Common examples are a Python venv's
// lib64 -> lib and pnpm's node_modules/<pkg> -> node_modules/.pnpm/...
func unhashedSkillDoc(skillDir string) (string, error) {
	root := skillDir
	if resolved, err := filepath.EvalSymlinks(skillDir); err == nil {
		root = resolved
	}
	var found string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if strings.EqualFold(d.Name(), skillDocName) {
				found = rel
				return fs.SkipAll
			}
			// A symlinked dir leading outside the skill may hold a SKILL.md
			// the walk never sees.
			if fi, serr := os.Stat(p); serr == nil && fi.IsDir() {
				if target, terr := filepath.EvalSymlinks(p); terr != nil || !isUnderDir(root, target) {
					found = rel
					return fs.SkipAll
				}
			}
			return nil
		}
		if d.IsDir() || !strings.EqualFold(d.Name(), skillDocName) {
			return nil
		}
		for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
			if config.IsExcludedDirName(part) {
				found = rel
				return fs.SkipAll
			}
		}
		return nil
	})
	return found, err
}
