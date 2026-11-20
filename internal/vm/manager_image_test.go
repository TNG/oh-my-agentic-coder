package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageRemoveCmd(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not installed")
	}
	bin := t.TempDir()
	script := `#!/bin/sh
echo "$*" >> "$OMAC_TEST_DOCKER_LOG"
case "$*" in
*"image inspect"*)
	case "$FAKE_DOCKER" in
	gone) echo "Error: No such image: $5" >&2; exit 1;;
	daemon) echo "Cannot connect to the Docker daemon" >&2; exit 1;;
	esac;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("PATH", bin)
	t.Setenv("OMAC_TEST_DOCKER_LOG", log)

	cases := []struct {
		name    string
		docker  string // what the stub's inspect answers
		wantErr string
		wantRm  bool
	}{
		{"image present", "", "", true},
		{"image gone", "gone", "", false},
		{"daemon down", "daemon", "Cannot connect to the Docker daemon", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := os.Remove(log); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			t.Setenv("FAKE_DOCKER", c.docker)
			out, err := exec.Command(sh, "-c", imageRemoveCmd("index.docker.io/library/alpine:latest")).CombinedOutput()
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("command failed: %v\n%s", err, out)
			case c.wantErr != "" && err == nil:
				t.Fatalf("command succeeded, want an error\n%s", out)
			case c.wantErr != "" && !strings.Contains(string(out), c.wantErr):
				t.Errorf("output %q lacks %q", out, c.wantErr)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			ranRm := strings.Contains(string(calls), "image rm index.docker.io/library/alpine:latest")
			if ranRm != c.wantRm {
				t.Errorf("image rm ran = %v, want %v (calls: %s)", ranRm, c.wantRm, calls)
			}
		})
	}
}
