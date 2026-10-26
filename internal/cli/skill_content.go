package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
// anchored in the host-only approval store, by splicing a --deny-literal for
// it into the sandbox argv. Each hidden skill gets one line on w naming the
// reason and the fix. Only omac's native sandbox understands the flag; on
// another backend the skill stays visible and the line says so.
//
// The path is passed literally because it comes from the disk, not from a
// user: a folder named "foo${UNSET}" must be hidden under that name, not
// expanded to "foo", and a "[" in it is not a glob.
//
// A skill root that cannot be listed is hidden whole rather than skipped: the
// sandbox can make a root unreadable (e.g. replace .claude/skills with a file),
// and that must not switch the check off for it or for the other roots.
func injectUntrustedSkillDenies(argv []string, w io.Writer, prefix string, prof config.SandboxProfile, workdirs ...string) []string {
	approvals := skilltrust.LoadApprovals()
	dirs, unreadable := omacSkillDirs(approvals, workdirs...)
	_, native := prof.PolicyRef()
	hide := func(path string, reason error) {
		if !native {
			fmt.Fprintf(w, "%s: %v. NOT hidden: only the built-in sandbox can hide it\n", prefix, reason)
			return
		}
		fmt.Fprintf(w, "%s: %v. Hidden from this session\n", prefix, reason)
		for _, t := range maskTargets(path, workdirs) {
			argv = injectSandboxFlag(argv, "--deny-literal", t)
		}
	}
	for _, r := range unreadable {
		hide(r.root, fmt.Errorf("cannot check the skills in %s: %w", r.root, r.err))
	}
	for _, sk := range dirs {
		if refusal := skillContentRefusal(approvals, sk.Name, sk.Dir); refusal != nil {
			hide(sk.Dir, refusal)
		}
	}
	return argv
}

// maskTargets returns the paths the sandbox must mask to hide path. The
// sandbox cannot mount a mask onto a symlink (bubblewrap >= 0.12 refuses,
// which aborts the whole launch), so a path that is or passes through a
// symlink is replaced by where it leads. Two spellings are returned because
// the sandbox only masks what it shows, and it may show the target under
// either one:
//   - the symlinks below the workdir (or home) resolved, the part above kept
//     as written, since that is how the workdir is granted;
//   - the fully resolved path.
//
// Example: with .opencode/skills/foo -> ../vendor/foo, the mask goes onto
// <workdir>/.opencode/vendor/foo. A spelling that still ends in a symlink is
// dropped. Without any usable spelling the path itself is returned.
func maskTargets(path string, workdirs []string) []string {
	base := string(filepath.Separator)
	if home, err := os.UserHomeDir(); err == nil && isUnderDir(home, path) {
		base = home
	}
	for _, wd := range workdirs {
		if isUnderDir(wd, path) {
			base = wd
			break
		}
	}
	var out []string
	add := func(p string) {
		if fi, err := os.Lstat(p); err != nil || fi.Mode()&fs.ModeSymlink != 0 || slices.Contains(out, p) {
			return
		}
		out = append(out, p)
	}
	if p, ok := resolveBelow(base, path); ok {
		add(p)
	}
	if p, err := filepath.EvalSymlinks(path); err == nil {
		add(p)
	}
	if len(out) == 0 {
		out = []string{path}
	}
	return out
}

// maxMaskSymlinkHops bounds symlink chains in resolveBelow, like the
// kernel's own limit.
const maxMaskSymlinkHops = 40

// resolveBelow resolves the symlinks in path that lie below base and keeps
// every other component as written. ok is false on a symlink loop or a
// broken link.
func resolveBelow(base, path string) (string, bool) {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return "", false
	}
	cur := base
	rest := strings.Split(rel, string(filepath.Separator))
	for hops := 0; len(rest) > 0; {
		name := rest[0]
		rest = rest[1:]
		if name == "" || name == "." {
			continue
		}
		next := filepath.Join(cur, name)
		if !isUnderDir(base, next) {
			// Outside base: keep the rest as written.
			return filepath.Join(append([]string{next}, rest...)...), true
		}
		fi, err := os.Lstat(next)
		if err != nil {
			return "", false
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		if hops++; hops > maxMaskSymlinkHops {
			return "", false
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(cur, target)
		}
		trel, err := filepath.Rel(base, target)
		if err != nil || !isUnderDir(base, target) {
			return filepath.Join(append([]string{target}, rest...)...), true
		}
		cur = base
		rest = append(strings.Split(trel, string(filepath.Separator)), rest...)
	}
	return cur, true
}

// unreadableSkillRoot is a skill root, or a folder below one, that exists
// but could not be listed.
type unreadableSkillRoot struct {
	root string
	err  error
}

// omacSkillDirs lists every omac skill dir under the given workdirs and the
// user-global skill roots that a harness loader might read SKILL.md from.
//
// Harness loaders search a skill root recursively and follow symlinks, so
// this does too: an approved "foo" moved to .opencode/skills/group/foo is
// still loaded, and still checked here. A dir counts as an omac skill when
// it has an omac.yaml, or when the host approval store knows its name (for a
// nested dir, only when it also holds a SKILL.md, so a plain skill's own
// "docs" folder is not mistaken for an approved skill "docs"). Deleting
// omac.yaml must not turn an approved skill into an unchecked plain one. The
// walk does not descend into an omac skill: skillContentRefusal checks
// everything below it.
//
// Unlike skillsource.Discover it keeps a dir even when a higher-priority dir
// has the same name, follows symlinked skill dirs, and covers every
// harness's base: a shadowed copy is still loaded, and OpenCode also reads
// .claude/skills.
func omacSkillDirs(approvals *skilltrust.Approvals, workdirs ...string) ([]skillsource.Entry, []unreadableSkillRoot) {
	var sources []skillsource.Source
	for _, wd := range workdirs {
		for _, h := range config.AllHarnesses() {
			sources = append(sources, skillsource.Sources(wd, h)...)
		}
	}
	seen := map[string]bool{}
	// descended holds the real paths of folders already searched, so a
	// symlink loop or a folder linked from two places is searched once.
	descended := map[string]bool{}
	var out []skillsource.Entry
	var unreadable []unreadableSkillRoot
	var budget int
	var walk func(dir string, kind string, nested bool) error
	walk = func(dir string, kind string, nested bool) error {
		if budget--; budget < 0 {
			return fmt.Errorf("it has more than %d folders", maxSkillScanDirs)
		}
		ents, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) && !nested {
			return nil
		}
		if err != nil {
			unreadable = append(unreadable, unreadableSkillRoot{root: dir, err: err})
			return nil
		}
		for _, ent := range ents {
			child := filepath.Join(dir, ent.Name())
			if seen[child] {
				continue
			}
			// os.Stat follows a symlinked dir.
			if fi, err := os.Stat(child); err != nil || !fi.IsDir() {
				continue
			}
			if isOmacSkillDir(approvals, child, nested) {
				seen[child] = true
				out = append(out, skillsource.Entry{Name: ent.Name(), Dir: child, Kind: kind})
				continue
			}
			real, err := filepath.EvalSymlinks(child)
			if err != nil || descended[real] {
				continue
			}
			descended[real] = true
			if err := walk(child, kind, true); err != nil {
				return err
			}
		}
		return nil
	}
	for _, src := range sources {
		if seen[src.Root] {
			continue
		}
		seen[src.Root] = true
		if real, err := filepath.EvalSymlinks(src.Root); err == nil {
			descended[real] = true
		}
		// A root too large to search is hidden whole, like an unreadable
		// one: a symlink to / must not stall the launch or end the check.
		budget = maxSkillScanDirs
		if err := walk(src.Root, src.Kind, false); err != nil {
			unreadable = append(unreadable, unreadableSkillRoot{root: src.Root, err: err})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, unreadable
}

// maxSkillScanDirs bounds how many folders omacSkillDirs searches per skill
// root. Real roots hold a few dozen skills; a plain skill with a large
// node_modules is the expensive case.
const maxSkillScanDirs = 50000

// isOmacSkillDir reports whether dir is an omac skill for omacSkillDirs. An
// approval store that cannot be read counts as knowing every name (fail
// closed); skillContentRefusal then reports the read error.
func isOmacSkillDir(approvals *skilltrust.Approvals, dir string, nested bool) bool {
	if _, err := os.Lstat(filepath.Join(dir, config.MetaFileName)); !errors.Is(err, os.ErrNotExist) {
		return true
	}
	if known, _ := approvals.HasName(filepath.Base(dir)); !known {
		return false
	}
	if !nested {
		return true
	}
	_, err := os.Lstat(filepath.Join(dir, skillDocName))
	return !errors.Is(err, os.ErrNotExist)
}

// skillContentRefusal reports why the instructions in skillDir must not reach
// the sandboxed harness, or nil when they match a host approval. It never
// consults the workdir registry (.opencode/sidecar.json): the sandbox can write
// that file, so a hash recorded there proves nothing.
func skillContentRefusal(approvals *skilltrust.Approvals, name, skillDir string) error {
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
	approved, err := approvals.IsApproved(name, h)
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
