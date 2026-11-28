package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecurityDesktopStateGrantExcludesProjectStore(t *testing.T) {
	tests := []struct {
		name     string
		stateDir func(home string) string
	}{
		{
			name: "macOS Application Support layout",
			stateDir: func(home string) string {
				return filepath.Join(home, "Library", "Application Support", "ai.opencode.desktop")
			},
		},
		{
			name: "XDG data layout",
			stateDir: func(home string) string {
				return filepath.Join(home, ".local", "share", "ai.opencode.desktop")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv("XDG_DATA_HOME", "")
			t.Setenv("XDG_STATE_HOME", "")

			stateDir := tc.stateDir(home)
			control := securityOutOfHomeDir(t)
			storeFile := securityWriteDesktopStore(t, stateDir, control)

			args, stderr := securityCaptureDesktopServe(t, home)
			allowed := map[string]bool{}
			for i, arg := range args {
				if arg == "--allow" && i+1 < len(args) {
					allowed[args[i+1]] = true
				}
			}

			stateGrant := filepath.Join(stateDir, "opencode")
			if !allowed[stateGrant] {
				t.Errorf("--for-opencode-desktop did not grant the OpenCode state directory %q: %v\n%s", stateGrant, args, stderr)
			}
			for path := range allowed {
				if securityPathWithin(path, storeFile) {
					t.Errorf("Desktop state grant %q contains the project store %q: %v\n%s", path, storeFile, args, stderr)
				}
			}
			if !allowed[control] {
				t.Errorf("control: Desktop worktree %q was not granted: %v\n%s", control, args, stderr)
			}
		})
	}
}

func securityCaptureDesktopServe(t *testing.T, home string) ([]string, string) {
	t.Helper()
	shortTmp, err := os.MkdirTemp("/tmp", "omac-sec-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shortTmp) })
	t.Setenv("TMPDIR", shortTmp)
	t.Setenv("XDG_RUNTIME_DIR", shortTmp)

	workdir := t.TempDir()
	files := t.TempDir()
	argsPath := filepath.Join(files, "args")
	readyPath := filepath.Join(files, "ready")
	releasePath := filepath.Join(files, "release")
	t.Setenv("OMAC_SERVE_TEST_ARGS", argsPath)
	t.Setenv("OMAC_SERVE_TEST_READY", readyPath)
	t.Setenv("OMAC_SERVE_TEST_RELEASE", releasePath)

	capturePath := filepath.Join(files, "capture")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$OMAC_SERVE_TEST_ARGS\"\n" +
		": > \"$OMAC_SERVE_TEST_READY\"\n" +
		"while [ ! -f \"$OMAC_SERVE_TEST_RELEASE\" ]; do sleep 0.01; done\n"
	if err := os.WriteFile(capturePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(home, ".config", "omac", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	configText := fmt.Sprintf("sandbox:\n  default_profile: capture\n  profiles:\n    capture:\n      command: [%q, %q, %q, %q]\n", capturePath, "--", "{{inner_cmd}}", "{{inner_args}}")
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	env, stderr := launchTestEnv(t, workdir)
	done := make(chan int, 1)
	go func() {
		done <- runServe([]string{"opencode", "--for-opencode-desktop", "--sandbox", "capture", "--inner", "/bin/true"}, env)
	}()
	released := false
	release := func() {
		if !released {
			released = true
			_ = os.WriteFile(releasePath, nil, 0o600)
		}
	}
	t.Cleanup(release)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		select {
		case code := <-done:
			t.Fatalf("runServe exited early with %d:\n%s", code, stderr())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for capture process:\n%s", stderr())
		}
		time.Sleep(10 * time.Millisecond)
	}

	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	release()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Errorf("runServe exit = %d, want ExitOK\n%s", code, stderr())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for runServe to exit:\n%s", stderr())
	}
	return strings.Fields(string(data)), stderr()
}

func securityOutOfHomeDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(wd, ".grant-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	home, _ := os.UserHomeDir()
	if securityPathWithin(home, dir) || securitySharedTempPath(dir) {
		t.Skipf("working directory %q cannot host an out-of-home control", wd)
	}
	return dir
}

func securityWriteDesktopStore(t *testing.T, stateDir string, worktrees ...string) string {
	t.Helper()
	type record struct {
		ID       string `json:"id"`
		Worktree string `json:"worktree"`
	}
	records := make([]record, 0, len(worktrees))
	for i, worktree := range worktrees {
		records = append(records, record{ID: fmt.Sprintf("security-%d", i), Worktree: worktree})
	}
	inner, err := json.Marshal(map[string]any{"value": records})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"globalSync.project": string(inner)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, "opencode.global.dat")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func securityPathWithin(root, path string) bool {
	if root == "" {
		return false
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func securitySharedTempPath(path string) bool {
	clean := filepath.Clean(path)
	for _, root := range []string{"/tmp", "/var/tmp", "/dev/shm", os.TempDir()} {
		if securityPathWithin(filepath.Clean(root), clean) {
			return true
		}
	}
	return false
}
