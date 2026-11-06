package sandboxrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/opencodestate"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

type desktopStoreFixture struct {
	home, runtime, known, unrelated string
	stores                          []string
	content                         map[string][]byte
	present                         bool
}

func newDesktopStoreFixture(t *testing.T, present bool) *desktopStoreFixture {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for env, dir := range map[string]string{"XDG_CONFIG_HOME": "config[1]$HOME", "XDG_DATA_HOME": "data[1]$HOME", "XDG_STATE_HOME": "state[1]$HOME"} {
		t.Setenv(env, filepath.Join(home, dir))
	}
	fx := &desktopStoreFixture{
		home: home, runtime: filepath.Join(os.Getenv("XDG_STATE_HOME"), "opencode"),
		known: filepath.Join(home, "known-project"), unrelated: filepath.Join(home, "unrelated-project"),
		stores: opencodestate.DesktopStorePaths(), content: map[string][]byte{}, present: present,
	}
	for _, d := range []string{fx.runtime, fx.known, fx.unrelated} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range fx.stores {
		if !isUnder(p, home) {
			t.Fatalf("test store outside temporary home: %s", p)
		}
		if !present {
			// Keep unused Desktop parent directories absent too: the agent
			// must not be able to plant one as a symlink into a project.
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		data := []byte(denyFileSecret)
		if filepath.Base(p) == "opencode.global.dat" {
			data, _ = json.Marshal(map[string]any{
				"globalSync.project": map[string]any{"value": []map[string]string{{"id": "known", "worktree": fx.known}}},
				"saved-credential":   denyFileSecret,
			})
		}
		fx.content[p] = data
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fx.checkDiscovery(t)
	return fx
}

func (fx *desktopStoreFixture) grants(t *testing.T, broadWrite bool) *Grants {
	t.Helper()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{
			AllowLiteral: []string{fx.runtime}, DenyLiteral: fx.stores,
			RequireProtectedPath: fx.stores,
		},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	if broadWrite {
		p.Filesystem.AllowLiteral = append(p.Filesystem.AllowLiteral, fx.home)
	} else {
		p.Filesystem.Read = []string{fx.home}
	}
	g, err := ResolveGrants(p, fx.known, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func (fx *desktopStoreFixture) script() string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	forged, _ := json.Marshal(map[string]any{"globalSync.project": map[string]any{"value": []map[string]string{{"id": "forged", "worktree": fx.unrelated}}}})
	var lines []string
	var dirs []string
	for _, p := range fx.stores {
		f, d := q(p), q(filepath.Dir(p))
		lines = append(lines,
			"cat "+f,
			"printf APPEND >> "+f,
			"printf TRUNCATE > "+f,
			"printf '%s' "+q(string(forged))+" > "+d+"/.replacement; mv -f "+d+"/.replacement "+f,
			"ln -sf /etc/hosts "+f,
			"rm -f "+f,
			"mv "+f+" "+f+".moved",
			"ln "+f+" "+f+".linked; cat "+f+".linked",
			"printf '%s' "+q(string(forged))+" > "+f,
		)
		for dir := filepath.Dir(p); isUnder(dir, fx.home); dir = filepath.Dir(dir) {
			dirs = append(dirs, dir)
		}
	}
	for _, dir := range dedupe(dirs) {
		lines = append(lines, "mv "+q(dir)+" "+q(dir+".moved"))
		for _, p := range fx.stores {
			if isUnder(p, dir) {
				rel, _ := filepath.Rel(dir, p)
				lines = append(lines, "cat "+q(filepath.Join(dir+".moved", rel)), "mkdir -p "+q(filepath.Dir(p))+"; printf '%s' "+q(string(forged))+" > "+q(p))
			}
		}
	}
	control := q(filepath.Join(fx.runtime, "runtime-control"))
	lines = append(lines, "printf RUNTIME-WRITABLE > "+control+" && cat "+control)
	return strings.Join(lines, "\n")
}

func (fx *desktopStoreFixture) writeScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(fx.runtime, ".desktop-store-probe.sh")
	if err := os.WriteFile(path, []byte(fx.script()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (fx *desktopStoreFixture) checkDiscovery(t *testing.T) {
	t.Helper()
	got, _, err := opencodestate.Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got, fx.unrelated) {
		t.Fatalf("restart discovery granted forged project: %q", got)
	}
	if fx.present && !slices.Contains(got, fx.known) {
		t.Fatalf("fixture lost known Desktop project: %q", got)
	}
}

func (fx *desktopStoreFixture) check(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, denyFileSecret) {
		t.Error("saved Desktop credentials were readable")
	}
	if !strings.Contains(out, "RUNTIME-WRITABLE") {
		t.Fatalf("runtime write control did not run; this is not enforcement evidence:\n%s", out)
	}
	for _, p := range fx.stores {
		got, err := os.ReadFile(p)
		if fx.present {
			if err != nil || string(got) != string(fx.content[p]) {
				t.Errorf("store changed/replaced %s: %v", p, err)
			}
		} else if !os.IsNotExist(err) {
			t.Errorf("absent store created: %s (%v)", p, err)
		}
		for _, suffix := range []string{".moved", ".linked"} {
			if _, err := os.Lstat(p + suffix); !os.IsNotExist(err) {
				t.Errorf("store escaped its denied spelling: %s", p+suffix)
			}
		}
	}
	fx.checkDiscovery(t)
}

func runDesktopStoreProbe(t *testing.T, argv []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Desktop sandbox probe did not execute successfully (%v):\n%s", err, out)
	}
	if ctx.Err() != nil {
		t.Fatal(fmt.Errorf("Desktop sandbox probe: %w", ctx.Err()))
	}
	return string(out)
}
