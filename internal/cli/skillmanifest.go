package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const skillManifestRelPath = ".opencode/skills.yaml"

// ManifestSkill is one entry in .opencode/skills.yaml.
// Either ID, Name, or both must be set; if both are set the marketplace
// response is checked and a warning is emitted when the canonical name
// does not match Name.
type ManifestSkill struct {
	ID      string `yaml:"id"`
	Name    string `yaml:"name"`
	Version *int   `yaml:"version,omitempty"`
}

// SkillManifest is the parsed form of .opencode/skills.yaml.
type SkillManifest struct {
	Version int             `yaml:"version"`
	Skills  []ManifestSkill `yaml:"skills"`
}

// LoadSkillManifest reads and parses the declarative skill manifest from
// <workdir>/.opencode/skills.yaml. Returns (nil, false, nil) when no
// manifest file exists — callers should skip the sync step entirely.
func LoadSkillManifest(workdir string) (*SkillManifest, bool, error) {
	path := filepath.Join(workdir, skillManifestRelPath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("read %s: %w", skillManifestRelPath, err)
	}
	var m SkillManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, true, fmt.Errorf("parse %s: %w", skillManifestRelPath, err)
	}
	if m.Version != 1 {
		return nil, true, fmt.Errorf("%s: unsupported version %d (expected 1)", skillManifestRelPath, m.Version)
	}
	for i, s := range m.Skills {
		if s.ID == "" && s.Name == "" {
			return nil, true, fmt.Errorf("%s: entry at index %d has neither id nor name", skillManifestRelPath, i)
		}
	}
	return &m, true, nil
}
