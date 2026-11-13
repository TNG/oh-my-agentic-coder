package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestDesktopProtectionBwrapRefusesWritableAncestors(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "present"}[present], func(t *testing.T) {
			root := t.TempDir()
			store := filepath.Join(root, "desktop", "drafts.sqlite")
			if err := os.MkdirAll(filepath.Dir(store), 0o700); err != nil {
				t.Fatal(err)
			}
			if present {
				if err := os.WriteFile(store, []byte("credentials"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			g := requiredProtectionGrants(t, root, store)
			if _, err := BuildBwrapArgv(g, []string{"true"}); err == nil || !strings.Contains(err.Error(), "narrow") {
				t.Fatalf("writable ancestor must refuse with remediation, got %v", err)
			}
		})
	}
}

func TestDesktopProtectionSBPLPinsEnclosingDirectories(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "desktop$HOME[1]", "drafts.sqlite-wal")
	g := requiredProtectionGrants(t, root, store)
	sbpl := GenerateSBPL(g)
	for _, dir := range []string{filepath.Dir(store), root} {
		for _, form := range pathForms(dir) {
			want := "(deny file-write-unlink (literal " + sbplQuote(form) + "))"
			if !strings.Contains(sbpl, want) {
				t.Errorf("missing directory-rename guard %s", want)
			}
			if strings.Contains(sbpl, "(deny file-write* (subpath "+sbplQuote(form)+"))") {
				t.Errorf("ancestor guard must not block runtime sibling writes: %s", form)
			}
		}
	}
}

func TestDesktopProtectionDoesNotChangeOrdinarySBPL(t *testing.T) {
	if got := GenerateSBPL(&Grants{Workdir: "/work", AllowPaths: []string{"/work"}, NetworkMode: sandboxprofile.ModeBlocked}); strings.Contains(got, "(deny file-write-unlink") {
		t.Fatal("enclosing-directory pins must be opt-in, not added to ordinary launches")
	}
}

func TestDesktopProtectionCannotBeUndoneBySBPLReadExceptions(t *testing.T) {
	store := "/System/Library/Keychains/drafts.sqlite"
	g := &Grants{Workdir: "/work", ProtectedPaths: []string{store}, RequiredProtectedPaths: []string{store}, NetworkMode: sandboxprofile.ModeBlocked}
	sbpl := GenerateSBPL(g)
	deny := "(deny file-read* (subpath " + sbplQuote(store) + "))"
	rootStoreAllow := `(allow file-read* (subpath "/System/Library/Keychains"))`
	if strings.LastIndex(sbpl, deny) <= strings.LastIndex(sbpl, rootStoreAllow) {
		t.Fatal("a platform read exception must not re-expose a trusted Desktop store")
	}
}

func TestDesktopProtectionBwrapAllowsRuntimeSibling(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "desktop", "opencode")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(filepath.Dir(runtimeDir), "drafts.sqlite")
	g := requiredProtectionGrants(t, runtimeDir, store)
	// The parent is read-only: existing stores must be masked, absent
	// companions cannot be created on the host. The runtime child stays RW.
	g.ReadPaths = append(g.ReadPaths, filepath.Dir(runtimeDir))
	if err := os.WriteFile(store, []byte("credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	argv, err := BuildBwrapArgv(g, []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(argv, "\n"), "--ro-bind\n/dev/null\n"+store) {
		t.Fatalf("existing store under read grant was not masked: %q", argv)
	}
}

func TestDesktopProtectionBwrapRefusesReadAlias(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "desktop")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "drafts.sqlite")
	g := requiredProtectionGrants(t, t.TempDir(), store)
	g.ReadPaths = append(g.ReadPaths, alias)
	if _, err := BuildBwrapArgv(g, []string{"true"}); err == nil {
		t.Fatal("alias grant could expose a store without its path mask")
	}
}

func TestDesktopProtectionCannotBeDisabledByLearn(t *testing.T) {
	g := requiredProtectionGrants(t, t.TempDir(), filepath.Join(t.TempDir(), "drafts.sqlite"))
	if _, err := BuildBwrapArgv(g.withUnrestrictedFilesystem(), []string{"true"}); err == nil {
		t.Fatal("required store protection must refuse learn mode")
	}
}

func requiredProtectionGrants(t *testing.T, grant, store string) *Grants {
	t.Helper()
	f, err := sandboxprofile.ParseFlags([]string{"--allow", grant, "--deny-literal", store, "--require-protected-path", store, "--", "true"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := sandboxprofile.Merge(&sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessNone},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}, f)
	g, err := ResolveGrants(p, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// These cross-platform policy tests simulate Linux mounts, not the
	// macOS baseline that grants the entire temporary directory writable.
	g.ReadPaths, g.WritePaths = nil, nil
	return g
}
