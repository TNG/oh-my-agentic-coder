package skillsource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// DiscoverRoot discovers only project-local skills through the admitted root.
// Globals are activated separately by serve and must not be scanned here.
// Entry.Dir is relative to root, never a pathname to reopen with os.Open.
func DiscoverRoot(root *os.Root, harness config.Harness) ([]Entry, error) {
	seen := map[string]bool{}
	var out []Entry
	for _, base := range harness.InScopeSkillsBases() {
		dir := filepath.Join("."+base, "skills")
		f, err := root.Open(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("skillsource: read %s: %w", dir, err)
		}
		entries, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			return nil, err
		}
		for _, ent := range entries {
			if !ent.IsDir() || seen[ent.Name()] {
				continue
			}
			p := filepath.Join(dir, ent.Name())
			if _, err := root.Stat(filepath.Join(p, config.MetaFileName)); err != nil {
				continue
			}
			seen[ent.Name()] = true
			out = append(out, Entry{Name: ent.Name(), Dir: p, Kind: "workdir"})
		}
	}
	return out, nil
}
