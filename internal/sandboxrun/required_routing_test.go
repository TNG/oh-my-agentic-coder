package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestDesktopProtectionRoutingResolvesDotDotAfterSymlinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"route/future", "actual/nested/leaf", "actual/nested/future"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	b := filepath.Join(root, "route", "b")
	if err := os.Symlink(filepath.Join(root, "actual", "nested", "leaf"), b); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(root, "a")
	if err := os.Symlink("route/b/../future", a); err != nil {
		t.Fatal(err)
	}
	dependencies, err := requiredRoutingDependencies(filepath.Join(a, "opencode.global.dat"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []requiredRoutingDependency{
		{path: b, symlink: true},
		{path: filepath.Join(root, "actual", "nested", "future")},
	} {
		if !slices.Contains(dependencies, want) {
			t.Errorf("missing actual routing dependency %+v: %+v", want, dependencies)
		}
	}
	if slices.Contains(dependencies, requiredRoutingDependency{path: filepath.Join(root, "route", "future")}) {
		t.Fatal("cleaned b/.. before following b: recorded the wrong future directory")
	}
}

func TestDesktopProtectionRefusesUnsafeSymlinkRouting(t *testing.T) {
	for _, layout := range []string{"dangling-target", "mutable-intermediate-directory-link", "mutable-intermediate-link", "mutable-link-before-dotdot", "mutable-directory-before-dotdot", "cycle"} {
		t.Run(layout, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			project := filepath.Join(root, "known-project")
			trusted := filepath.Join(root, "trusted")
			final := filepath.Join(root, "outside-project", "desktop")
			for _, dir := range []string{project, trusted, final} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(trusted, "ai.opencode.desktop.beta")
			target := filepath.Join(project, "future")
			switch layout {
			case "mutable-intermediate-directory-link":
				hop := filepath.Join(project, "routing-hop")
				if err := os.Symlink(filepath.Dir(final), hop); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(hop, "desktop")
			case "mutable-intermediate-link":
				target = filepath.Join(project, "routing-hop")
				if err := os.Symlink(final, target); err != nil {
					t.Fatal(err)
				}
			case "mutable-link-before-dotdot":
				// The kernel resolves b before ..: this reaches outside/future,
				// not project/future. Both exist so cleaning the target early
				// cannot accidentally make the test pass on a dangling link.
				for _, dir := range []string{filepath.Join(project, "future"), filepath.Join(filepath.Dir(final), "branch"), filepath.Join(filepath.Dir(final), "future")} {
					if err := os.Mkdir(dir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(filepath.Join(filepath.Dir(final), "branch"), filepath.Join(project, "b")); err != nil {
					t.Fatal(err)
				}
				target = project + "/b/../future" // Deliberately do not filepath.Join/Clean.
			case "mutable-directory-before-dotdot":
				for _, dir := range []string{filepath.Join(project, "ordinary"), filepath.Join(filepath.Dir(final), "future")} {
					if err := os.Mkdir(dir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				// This ordinary directory could be replaced with a symlink.
				// It is a routing dependency even though .. leaves it behind.
				target = project + "/ordinary/../../outside-project/future"
			case "cycle":
				target = link
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			store := filepath.Join(link, "opencode.global.dat")
			g := &Grants{Workdir: project, AllowPaths: []string{project}, ProtectedPaths: []string{store}, RequiredProtectedPaths: []string{store}, NetworkMode: sandboxprofile.ModeBlocked}
			for _, linux := range []bool{false, true} {
				err := g.validateRequiredProtection(linux)
				if err == nil || !strings.Contains(err.Error(), "symlink") {
					t.Errorf("unsafe %s routing accepted (linux=%v): %v", layout, linux, err)
				}
			}
		})
	}
}

func TestDesktopProtectionSBPLBlocksMissingParentSymlinkCreation(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "future-state")
	parent := filepath.Join(first, "desktop")
	store := filepath.Join(parent, "opencode.global.dat")
	g := &Grants{Workdir: root, AllowPaths: []string{root}, ProtectedPaths: []string{store}, RequiredProtectedPaths: []string{store}, NetworkMode: sandboxprofile.ModeBlocked}
	if err := g.validateRequiredProtection(false); err != nil {
		t.Fatal(err)
	}
	sbpl := GenerateSBPL(g)
	for _, dir := range []string{first, parent} {
		for _, form := range pathForms(dir) {
			want := "(deny file-write-create (literal " + sbplQuote(form) + "))"
			if !strings.Contains(sbpl, want) {
				t.Errorf("absent dependency can be planted as a symlink after launch; missing %s", want)
			}
			if strings.Contains(sbpl, "(deny file-write* (subpath "+sbplQuote(form)+"))") {
				t.Errorf("only dependency creation needs an extra guard, not a subtree deny: %s", form)
			}
		}
	}
}

func TestDesktopProtectionKeepsImmutableSymlinkRouting(t *testing.T) {
	root := t.TempDir() // Also exercises /var -> /private/var on macOS.
	actual := filepath.Join(root, "actual")
	project := filepath.Join(root, "project")
	for _, dir := range []string{actual, project} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "immutable-alias")
	if err := os.Symlink(actual, alias); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(alias, "future-desktop", "opencode.global.dat")
	g := &Grants{Workdir: project, AllowPaths: []string{project}, ProtectedPaths: []string{store}, RequiredProtectedPaths: []string{store}, NetworkMode: sandboxprofile.ModeBlocked}
	for _, linux := range []bool{false, true} {
		if err := g.validateRequiredProtection(linux); err != nil {
			t.Errorf("immutable routing refused (linux=%v): %v", linux, err)
		}
	}
	sbpl := GenerateSBPL(g)
	// Check the requested spelling and its real target (/private/var on
	// macOS), not an extra equivalent /var spelling of that target.
	for _, form := range pathForms(filepath.Join(alias, "future-desktop")) {
		if !strings.Contains(sbpl, "(deny file-write-create (literal "+sbplQuote(form)+"))") {
			t.Errorf("missing dependency reached through an immutable alias was not guarded: %s", form)
		}
	}
}
