//go:build linux

package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestGitServiceLaunchIntegration(t *testing.T) {
	probe := exec.Command("/usr/bin/bwrap", "--unshare-net", "--unshare-pid", "--ro-bind", "/usr", "/usr", "--ro-bind", "/lib", "/lib", "--ro-bind", "/lib64", "/lib64", "--proc", "/proc", "--dev", "/dev", "--", "/usr/bin/git", "--version")
	if out, err := probe.CombinedOutput(); err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" || os.Getenv("OMAC_REQUIRE_GIT_WORKER_TESTS") == "1" {
			t.Fatalf("worker unavailable: %v %s", err, out)
		}
		t.Skipf("worker namespaces unavailable: %v %s", err, out)
	}
	binary := filepath.Join(t.TempDir(), "omac")
	build := exec.Command("go", "build", "-o", binary, "./cmd/omac")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	for _, answer := range []string{"Allow once", "Allow for this session", "Deny"} {
		t.Run(answer, func(t *testing.T) {
			// Keep the Unix socket below its platform path-length limit.
			root, err := os.MkdirTemp("/tmp", "omac-git-test-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			home, repo := filepath.Join(root, "home"), filepath.Join(root, "repo")
			for _, p := range []string{home, repo, filepath.Join(home, ".config", "omac", "sandbox-profiles")} {
				if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			env := []string{"PATH=/usr/bin:/bin", "HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"), "TMPDIR=/tmp", "OMAC_APPROVAL_STUB=" + answer, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid"}
			git := func(args ...string) string {
				t.Helper()
				c := exec.Command("/usr/bin/git", args...)
				c.Dir = repo
				c.Env = env
				out, err := c.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "-q", "-b", "main")
			if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".env\nbuild-output\n.opencode/\n.claude/\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "program.c"), []byte("#include <stdio.h>\nint main(void) { puts(\"hello\"); return 0; }\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("commit", "-qm", "base")
			original := git("rev-parse", "HEAD")
			originalConfig, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("fake-private-canary"), 0600); err != nil {
				t.Fatal(err)
			}
			protectedParent := filepath.Join(repo, "deployment", "local")
			if err := os.MkdirAll(protectedParent, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(protectedParent, ".env"), []byte("nested-private-canary"), 0600); err != nil {
				t.Fatal(err)
			}
			policy := sandboxprofile.DefaultProfile()
			policy.Network.Mode = sandboxprofile.ModeOpen
			policy.Filesystem.Read = append(policy.Filesystem.Read, binary)
			data, _ := json.Marshal(policy)
			if err := os.WriteFile(filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			script := `set -eu
if echo poison >> .git/config; then exit 31; fi
if mv deployment moved; then exit 38; fi
printf 'one\n' > source
if /usr/bin/git add source; then exit 32; fi
cc program.c -o build-output
test "$(./build-output)" = hello
"` + binary + `" git status
`
			if answer == "Deny" {
				script += `if git add -- source 2> denial; then exit 34; fi
grep -q 'HTTP 403' denial
echo DENIAL-PRESERVED
`
			} else {
				script += `if git add -- .env; then exit 35; fi
if git add -- deployment/local/.env; then exit 39; fi
mkdir subdir
(cd subdir; git add -- ../source)
git add -- source
git commit -m first --signoff
git branch topic
git switch topic
printf 'two\n' > source
git add -- source
git commit -m second
git switch main
test "$(cat source)" = one
git status --porcelain
if git push origin main; then exit 36; fi
echo WORKFLOW-PASSED
`
			}
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "--workdir", repo, "start", "--inner", "/bin/sh", "--git-service=prototype", "--git-user-name=Test", "--git-user-email=test@example.invalid", "--git-wrapper", "--", "-c", script)
			cmd.Env = env
			cmd.WaitDelay = 3 * time.Second
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("launch: %v\n%s", err, out)
			}
			if answer == "Deny" {
				if !strings.Contains(string(out), "DENIAL-PRESERVED") || git("rev-parse", "HEAD") != original {
					t.Fatalf("denied launch mutated Git: %s", out)
				}
			} else {
				if !strings.Contains(string(out), "WORKFLOW-PASSED") || git("show", "HEAD:source") != "one" || git("show", "topic:source") != "two" {
					t.Fatalf("bad workflow result: %s", out)
				}
				if !strings.Contains(git("log", "-1", "--format=%B"), "Signed-off-by: Test <test@example.invalid>") {
					t.Fatal("missing sign-off")
				}
			}
			git("status", "--porcelain")
			if config, err := os.ReadFile(filepath.Join(repo, ".git", "config")); err != nil || string(config) != string(originalConfig) {
				t.Fatal("service changed host configuration")
			}
			cmd = exec.CommandContext(ctx, binary, "--workdir", repo, "start", "--inner", "/bin/sh", "--git-service=prototype", "--git-user-name=Test", "--git-user-email=test@example.invalid", "--git-wrapper", "--", "-c", `set -eu; git status --porcelain; if touch .git/reopened; then exit 37; fi`)
			cmd.Env = env
			cmd.WaitDelay = 3 * time.Second
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("second session: %v\n%s", err, out)
			}
		})
	}
}
