package sandboxprofile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// boolPtr is a tiny helper for NetworkPrompt.Enabled.
func boolPtr(b bool) *bool { return &b }

// DefaultProfile returns the compiled-in default settings. It is used
// only as the template for the scaffolded
// ~/.config/omac/sandbox-profiles/default.json — once that file exists,
// the file is authoritative.
//
// Harness-specific dirs (config, state, sessions) are NOT here — they
// are declared per-harness via Harness.SandboxDirs and injected at
// launch time. This profile contains platform-level system paths and
// common toolchain/cache dirs.
func DefaultProfile() *Profile {
	return &Profile{
		Meta:    Meta{Name: "default"},
		Workdir: Workdir{Access: AccessReadWrite},
		Filesystem: Filesystem{
			Allow: []string{
				"~/.cache",
				"~/Library/Caches",
				"~/go",
				"~/.rustup",
				"~/.cargo",
			},
			Read: []string{
				"~/.gitconfig",
				"~/.gitignore_global",
				"~/.nvm",
				"~/.bun/bin",
				"~/.bun/install/global/node_modules/opencode-ai",
				// Shared neutral skills base (agentskills.io). In scope for every
				// harness via InScopeSkillsBases, so a guidance-only SKILL.md
				// placed under the shared global root is readable inside the
				// sandbox. Per-harness global skills dirs are granted RW via
				// Harness.SandboxDirs at launch.
				"~/.config/agents/skills",
				"~/.agents/skills",
			},
		},
		Network: Network{
			Mode:            ModeFiltered,
			ListenPort:      []int{4097},
			AllowTCPConnect: []int{22},
			NetworkPrompt: &NetworkPrompt{
				Enabled:           boolPtr(true),
				PromptTimeoutSecs: DefaultPromptTimeoutSecs,
				OnUnavailable:     OnUnavailableDeny,
			},
		},
	}
}

// ProfileDir returns ~/.config/omac/sandbox-profiles.
func ProfileDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "omac", "sandbox-profiles"), nil
}

// ProfilePath returns the on-disk path for a named profile.
func ProfilePath(name string) (string, error) {
	dir, err := ProfileDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// PagesPath returns the sibling pages file for a profile path or name:
// <dir>/<name>.pages.json.
func PagesPath(profilePath string) string {
	return strings.TrimSuffix(profilePath, ".json") + ".pages.json"
}

// Resolve loads a profile reference:
//   - a path (contains a separator or ends in .json): load that file;
//   - otherwise ~/.config/omac/sandbox-profiles/<ref>.json.
//
// An empty ref means "default". On first use the default profile file
// is scaffolded from DefaultProfile (pretty-printed) and then loaded.
// Returns the profile and the path it was loaded from (the path is ""
// for explicit-path refs whose pages file should sit next to them —
// in that case the returned path is the explicit path itself).
func Resolve(ref string) (*Profile, string, error) {
	if ref == "" {
		ref = "default"
	}
	if strings.ContainsRune(ref, os.PathSeparator) || strings.HasSuffix(ref, ".json") {
		p, err := loadFile(ref)
		return p, ref, err
	}
	path, err := ProfilePath(ref)
	if err != nil {
		return nil, "", fmt.Errorf("resolve sandbox profile dir: %w", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		if !os.IsNotExist(statErr) {
			return nil, "", fmt.Errorf("stat sandbox profile %s: %w", path, statErr)
		}
		if ref != "default" {
			return nil, "", fmt.Errorf("sandbox profile %q not found (expected %s)", ref, path)
		}
		// First start: scaffold default.json so the user has an
		// editable copy, then load it back (round-trip keeps the file
		// authoritative).
		if err := WriteProfile(path, DefaultProfile()); err != nil {
			return nil, "", fmt.Errorf("scaffold default sandbox profile: %w", err)
		}
	}
	p, err := loadFile(path)
	return p, path, err
}

// WriteProfile writes a profile pretty-printed (2-space indent,
// trailing newline) atomically.
func WriteProfile(path string, p *Profile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := MarshalPretty(p)
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

// MarshalPretty renders any value as indented JSON with a trailing
// newline — the house style for every JSON file omac writes.
func MarshalPretty(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// writeAtomic writes data via temp-file + rename.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func loadFile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read sandbox profile %s: %w", path, err)
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}
