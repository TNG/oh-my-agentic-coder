package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestRunEmitsImplicitProxyPortNoticeOnlyForLinuxKernelFiltering(t *testing.T) {
	tests := []struct {
		name        string
		goos        string
		mode        string
		enforcement string
		wantNotice  bool
	}{
		{name: "linux filtered kernel", goos: "linux", mode: sandboxprofile.ModeFiltered, enforcement: sandboxprofile.EnforceKernel, wantNotice: true},
		{name: "non-Linux filtered kernel", goos: "darwin", mode: sandboxprofile.ModeFiltered, enforcement: sandboxprofile.EnforceKernel},
		{name: "Linux blocked kernel", goos: "linux", mode: sandboxprofile.ModeBlocked, enforcement: sandboxprofile.EnforceKernel},
		{name: "Linux filtered env-only", goos: "linux", mode: sandboxprofile.ModeFiltered, enforcement: sandboxprofile.EnforceEnvOnly},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			profile := sandboxprofile.DefaultProfile()
			profile.Network.Mode = tc.mode
			profile.Network.Enforcement = tc.enforcement
			requireRunNoticeCapabilities(t, profile)

			temp := t.TempDir()
			home := filepath.Join(temp, "home")
			tmp := filepath.Join(temp, "tmp")
			for _, dir := range []string{home, tmp} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("TMPDIR", tmp)
			t.Setenv("OMAC_CACHE_DIR", "")
			t.Setenv("OMAC_CACHE_MODE", "")
			t.Setenv("OMAC_BASE", "")

			profilePath := filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json")
			if err := sandboxprofile.WriteProfile(profilePath, profile); err != nil {
				t.Fatal(err)
			}

			var stderr strings.Builder
			childCalls := 0
			workdir := filepath.Join(temp, "workdir")
			if err := os.Mkdir(workdir, 0o700); err != nil {
				t.Fatal(err)
			}
			code := run(Options{
				Flags:   &sandboxprofile.Flags{InnerArgv: []string{"true"}},
				Workdir: workdir,
				Stderr:  &stderr,
			}, tc.goos, func(argv, env []string, childWorkdir string, onReady func(int)) (int, error) {
				childCalls++
				if len(argv) == 0 || childWorkdir != workdir {
					t.Errorf("child launch argv=%v workdir=%q; want a command launched in %q", argv, childWorkdir, workdir)
				}
				return 29, nil
			})
			if code != 29 || childCalls != 1 {
				t.Fatalf("launch returned %d after %d child calls, want 29 after one", code, childCalls)
			}

			got := strings.Count(stderr.String(), "Linux Landlock is address-blind: the implicit proxy port grant")
			want := 0
			if tc.wantNotice {
				want = 1
			}
			if got != want {
				t.Errorf("implicit proxy notice count = %d, want %d; stderr:\n%s", got, want, stderr.String())
			}
			if tc.wantNotice && !strings.Contains(stderr.String(), "notice: [low] network.mode:") {
				t.Errorf("launch notice does not follow the LOW audit output convention:\n%s", stderr.String())
			}
		})
	}
}
