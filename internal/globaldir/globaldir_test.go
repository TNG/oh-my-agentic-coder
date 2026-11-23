package globaldir

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirHonorsXDGConfigHome(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := Dir(); got != "/xdg/omac" {
		t.Fatalf("Dir() = %q, want /xdg/omac", got)
	}
}

func TestDirFallsBackToHome(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := Dir(); got != "/home/u/.config/omac" {
		t.Fatalf("Dir() = %q, want /home/u/.config/omac", got)
	}
}

func TestNoHardCodedConfigDirOutsideHelper(t *testing.T) {
	const needle = `".config", "omac"`
	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "website":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) == "globaldir" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), needle) {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("resolve the omac config directory with globaldir.Dir() so $XDG_CONFIG_HOME is honored; hard-coded in: %v", offenders)
	}
}
