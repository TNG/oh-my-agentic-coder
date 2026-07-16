//go:build linux

package sandboxrun

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// workflowTestHome builds a throwaway HOME populated with the dirs/files
// DefaultProfile() grants (~/.cache, ~/go, ~/.gitconfig), so the battery
// below exercises the real shipped default rather than the live
// developer machine's actual home directory.
func workflowTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, dir := range []string{".cache", "go"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"),
		[]byte("[user]\n\tname = Test\n\temail = test@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return home
}

// workflowDefaultGrants resolves grants from the exact compiled-in
// sandboxprofile.DefaultProfile() — the template that ships as
// ~/.config/omac/sandbox-profiles/default.json — rather than a
// hand-built Profile. A change to DefaultProfile() or baseline.go that
// breaks a real workflow shows up here, as opposed to a snapshot test
// that would only show that *something* in the list changed.
func workflowDefaultGrants(t *testing.T, wd string) *Grants {
	t.Helper()
	p := sandboxprofile.DefaultProfile()
	g, err := ResolveGrants(p, wd, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// TestIntegrationWorkflowGitBattery proves the ordinary git lifecycle a
// coding agent runs every session (init, add, commit, log, diff) still
// works end-to-end under the real default profile and a real bwrap
// sandbox.
func TestIntegrationWorkflowGitBattery(t *testing.T) {
	requireBwrap(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	workflowTestHome(t)
	wd := t.TempDir()
	g := workflowDefaultGrants(t, wd)

	script := strings.Join([]string{
		"set -e",
		"cd " + wd,
		"git init -q",
		"git config user.email t@example.com",
		"git config user.name Test",
		"echo hi > a.txt",
		"git add a.txt",
		"git commit -qm init",
		"echo bye >> a.txt",
		"git add a.txt",
		"git commit -qm update",
		"git log --oneline | wc -l",
	}, " && ")
	out, code := runBwrapped(t, g, "/bin/sh", "-c", script)
	if code != 0 {
		t.Fatalf("git workflow failed, exit %d: %s", code, out)
	}
	// Take the last whitespace-separated token rather than comparing the
	// whole trimmed output — robust against any shell/sandbox diagnostic
	// noise CombinedOutput() mixes in ahead of the real `wc -l` result
	// (see the darwin variant, where a narrowly-scoped Seatbelt grant
	// causes /bin/sh to print such a diagnostic).
	fields := strings.Fields(out)
	got := ""
	if len(fields) > 0 {
		got = fields[len(fields)-1]
	}
	if got != "2" {
		t.Errorf("expected 2 commits in git log, got %q (full output: %s)", got, out)
	}
}

// TestIntegrationWorkflowGitconfigReadable proves the non-secret
// ~/.gitconfig DefaultProfile() explicitly grants stays readable — a
// toolchain (git itself, editors) reads it constantly, and it must not
// be caught by a future credential-protection change aimed at files
// like ~/.git-credentials.
func TestIntegrationWorkflowGitconfigReadable(t *testing.T) {
	requireBwrap(t)
	home := workflowTestHome(t)
	wd := t.TempDir()
	g := workflowDefaultGrants(t, wd)

	out, code := runBwrapped(t, g, "/bin/cat", filepath.Join(home, ".gitconfig"))
	if code != 0 {
		t.Fatalf("~/.gitconfig must stay readable under the default profile: exit %d: %s", code, out)
	}
	if !strings.Contains(out, "Test") {
		t.Errorf("unexpected ~/.gitconfig content: %s", out)
	}
}

// TestIntegrationWorkflowGoBuild proves a trivial `go build` inside the
// workdir works under the default profile: it needs GOCACHE
// ($HOME/.cache/go-build, covered by DefaultProfile()'s ~/.cache grant)
// and GOPATH ($HOME/go, covered by the ~/go grant) to actually be
// writable, not just the workdir itself.
func TestIntegrationWorkflowGoBuild(t *testing.T) {
	requireBwrap(t)
	home := workflowTestHome(t)
	wd := t.TempDir()
	g := workflowDefaultGrants(t, wd)

	// Confirm `go` is visible inside the sandbox before asserting on the
	// build itself — its install location varies by environment (apt vs.
	// a version manager) and that's an environment property, not a
	// regression this test is meant to catch.
	if out, code := runBwrapped(t, g, "/bin/sh", "-c", "command -v go"); code != 0 || strings.TrimSpace(out) == "" {
		t.Skip("go not visible inside the sandbox on this runner")
	}

	if err := os.WriteFile(filepath.Join(wd, "go.mod"), []byte("module workflowtest\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainSrc := "package main\n\nfunc main() { println(\"ok\") }\n"
	if err := os.WriteFile(filepath.Join(wd, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf("cd %s && HOME=%s go build -o out ./...", wd, home)
	out, code := runBwrapped(t, g, "/bin/sh", "-c", script)
	if code != 0 {
		t.Fatalf("go build failed under default profile: exit %d: %s", code, out)
	}
}

// TestIntegrationWorkflowInterpretersRunnable smoke-tests that common
// scripting interpreters, if present on the runner, still execute a
// trivial no-op under the default profile's baseline Read grants
// (/usr, /bin et al.). Skips per-interpreter when absent — this
// documents the current behavior for whatever is installed rather than
// asserting a specific toolchain must exist.
func TestIntegrationWorkflowInterpretersRunnable(t *testing.T) {
	requireBwrap(t)
	workflowTestHome(t)
	wd := t.TempDir()
	g := workflowDefaultGrants(t, wd)

	cases := []struct {
		name string
		argv []string
	}{
		{"python3", []string{"python3", "-c", "print('ok')"}},
		{"node", []string{"node", "-e", "console.log('ok')"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := exec.LookPath(c.argv[0]); err != nil {
				t.Skipf("%s not installed", c.argv[0])
			}
			out, code := runBwrapped(t, g, c.argv...)
			if code != 0 || !strings.Contains(out, "ok") {
				t.Errorf("%s no-op failed under default profile: exit %d: %s", c.name, code, out)
			}
		})
	}
}

// TestIntegrationWorkflowDefaultProfileOpenPort proves that the shipped
// default profile's ModeFiltered network posture, combined with a
// legitimate open_port grant a user adds for a local dev server, still
// works together — mirrors TestIntegrationStage2LandlockPorts but
// pinned to sandboxprofile.DefaultProfile() as the base rather than a
// hand-built Profile, so a change to the default profile's Network
// stanza is exercised here too.
func TestIntegrationWorkflowDefaultProfileOpenPort(t *testing.T) {
	requireBwrap(t)
	if !LandlockNetSupported() {
		t.Skipf("Landlock ABI %d < 4", LandlockABI())
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not installed")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "workflow-ok")
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	// Build omac using the real host HOME/GOPATH, before workflowTestHome
	// below overrides HOME for the sandboxed run — otherwise `go build`
	// writes its module cache (read-only files) into the throwaway HOME,
	// which t.TempDir() cleanup then cannot remove.
	omac := filepath.Join(t.TempDir(), "omac")
	build := exec.Command("go", "build", "-o", omac, "github.com/tngtech/oh-my-agentic-coder/cmd/omac")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build omac: %v\n%s", err, out)
	}

	workflowTestHome(t)
	wd := t.TempDir()
	p := sandboxprofile.DefaultProfile()
	p.Network.OpenPort = []int{port}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.ReadPaths = append(g.ReadPaths, filepath.Dir(omac))

	stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
	argvTail := append(append([]string{}, stage2...), "--", curl, "-sS", "--max-time", "3",
		fmt.Sprintf("http://127.0.0.1:%d/", port))
	argv, err := BuildBwrapArgv(g, argvTail)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("default profile + open_port loopback failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "workflow-ok") {
		t.Errorf("unexpected response: %s", out)
	}
}
