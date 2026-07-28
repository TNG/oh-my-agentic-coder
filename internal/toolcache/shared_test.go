package toolcache

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDescribeSharedIsWorkdirIndependent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a, err := DescribeShared()
	if err != nil {
		t.Fatalf("DescribeShared: %v", err)
	}
	b, err := DescribeShared()
	if err != nil {
		t.Fatalf("DescribeShared: %v", err)
	}
	if a.Dir != b.Dir {
		t.Errorf("shared dir not stable: %q vs %q", a.Dir, b.Dir)
	}
	if a.Domain != DomainShared {
		t.Errorf("domain = %q, want %q", a.Domain, DomainShared)
	}
	if a.CanonicalPath != "" {
		t.Errorf("shared scope should have no canonical path, got %q", a.CanonicalPath)
	}
}

func TestDomainConfigKeysOnPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfgA := filepath.Join(t.TempDir(), "config.yaml")
	cfgB := filepath.Join(t.TempDir(), "config.yaml")
	for _, p := range []string{cfgA, cfgB} {
		if err := os.WriteFile(p, []byte("cache:\n  scope: config\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	a, err := DescribePersistent(DomainConfig, cfgA)
	if err != nil {
		t.Fatalf("describe cfgA: %v", err)
	}
	b, err := DescribePersistent(DomainConfig, cfgB)
	if err != nil {
		t.Fatalf("describe cfgB: %v", err)
	}
	if a.Dir == b.Dir {
		t.Errorf("distinct config paths share a cache dir: %q", a.Dir)
	}

	shared, err := DescribeShared()
	if err != nil {
		t.Fatalf("DescribeShared: %v", err)
	}
	if a.Dir == shared.Dir {
		t.Errorf("config scope collided with shared scope: %q", a.Dir)
	}
}
