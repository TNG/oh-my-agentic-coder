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
func injectUntrustedSkillDenies(argv []string, w io.Writer, prefix string, prof config.SandboxProfile, workdirs ...string) []string {
	dirs, err := omacSkillDirs(workdirs...)
	if err != nil {
		fmt.Fprintf(w, "%s: cannot check skill instructions: %v\n", prefix, err)
		return argv
	}
	_, native := prof.PolicyRef()
	for _, sk := range dirs {
		refusal := skillContentRefusal(sk.Name, sk.Dir)
		if refusal == nil {
			continue
		}
		if !native {
			fmt.Fprintf(w, "%s: %v. NOT hidden: only the built-in sandbox can hide it\n", prefix, refusal)
			continue
		}
		fmt.Fprintf(w, "%s: %v. Hidden from this session\n", prefix, refusal)
		argv = injectSandboxFlag(argv, "--deny", sk.Dir)
	}
	return argv
}

// omacSkillDirs lists every omac skill dir (one with an omac.yaml) under the
// given workdirs and the user-global skill roots that a harness loader might
// read SKILL.md from. Unlike skillsource.Discover it keeps a dir even when a
// higher-priority dir has the same name, follows symlinked skill dirs, and
// covers every harness's base: a shadowed copy is still loaded, and OpenCode
// also reads .claude/skills.
func omacSkillDirs(workdirs ...string) ([]skillsource.Entry, error) {
	var sources []skillsource.Source
	for _, wd := range workdirs {
		for _, h := range config.AllHarnesses() {
			sources = append(sources, skillsource.Sources(wd, h)...)
		}
	}
	seen := map[string]bool{}
	var out []skillsource.Entry
	for _, src := range sources {
		ents, err := os.ReadDir(src.Root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", src.Root, err)
		}
		for _, ent := range ents {
			dir := filepath.Join(src.Root, ent.Name())
			if seen[dir] {
				continue
			}
			// os.Stat follows a symlinked skill dir.
			if fi, err := os.Stat(filepath.Join(dir, config.MetaFileName)); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			seen[dir] = true
			out = append(out, skillsource.Entry{Name: ent.Name(), Dir: dir, Kind: src.Kind})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// skillContentRefusal reports why the instructions in skillDir must not reach
// the sandboxed harness, or nil when they match a host approval. It never
// consults the workdir registry (.opencode/sidecar.json): the sandbox can write
// that file, so a hash recorded there proves nothing.
func skillContentRefusal(name, skillDir string) error {
	rel, err := unhashedSkillDoc(skillDir)
	if err != nil {
		return fmt.Errorf("skill %q: cannot check %s: %w", name, skillDir, err)
	}
	if rel != "" {
		return fmt.Errorf("skill %q: %s is not covered by its approval (symlinks and files under dirs like build/ or node_modules/ are not hashed); remove it", name, filepath.Join(skillDir, rel))
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
			// A symlinked SKILL.md, or a symlinked dir that may hold one.
			if fi, serr := os.Stat(p); strings.EqualFold(d.Name(), skillDocName) || (serr == nil && fi.IsDir()) {
				found = rel
				return fs.SkipAll
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
