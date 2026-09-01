package cli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxbrief"
	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

// briefingInjection reports whether omac should inject the sandbox briefing
// and, if so, the resolved text (override is config.yaml sandbox.briefing;
// empty uses the embedded default).
//
// It injects only when a real sandbox wraps the inner command AND that command
// is the harness's own agent binary. The latter excludes --inner-overridden
// commands (e.g. `--inner bash`), so the briefing never lands on the wrong
// process.
//
// The cache guidance paragraph is always appended (default or custom
// briefing) because hardcoded host caches are denied by the sandbox —
// an override must not be able to suppress it.
func briefingInjection(noSandbox bool, inner []string, harness config.Harness, override string, cacheScope *toolcache.Scope) (string, bool) {
	if noSandbox || len(inner) == 0 || len(harness.InnerCmd) == 0 {
		return "", false
	}
	if inner[0] != harness.InnerCmd[0] {
		return "", false
	}
	text := sandboxbrief.Resolve(override)
	var dir string
	var mode toolcache.Mode
	if cacheScope != nil {
		dir = cacheScope.Dir
		mode = cacheScope.Mode
	}
	text += sandboxbrief.CacheGuidance(dir, mode)
	return text, true
}

// removeBriefingFile deletes a workdir briefing file written by a harness's
// BriefingFileFunc (e.g. codewhale's .codewhale/rules/omac-sandbox-briefing.md)
// and best-effort prunes the parent directories omac may have created for it.
// All operations are best-effort: os.Remove only removes empty directories, so
// a user's own files under .codewhale/rules or .codewhale are never touched.
func removeBriefingFile(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
	// Prune up to two now-empty parents (e.g. .codewhale/rules, then
	// .codewhale). os.Remove fails on a non-empty dir, so this is safe.
	dir := filepath.Dir(path)
	_ = os.Remove(dir)
	_ = os.Remove(filepath.Dir(dir))
}

// gitExcludeBriefing makes git ignore a workdir briefing file written by a
// harness's BriefingFileFunc, by appending its workdir-relative path to
// <workdir>/.git/info/exclude. Without this, an agent whose own workflow runs
// `git add -A && git commit` would stage and commit the briefing (and a later
// removeBriefingFile then leaves a staged deletion of a now-tracked file).
//
// .git/info/exclude is the repo-local, never-committed ignore list — this
// touches neither the user's .gitignore nor the index. It is written BEFORE
// the agent launches, so the entry persists even if omac is SIGKILLed (which
// the deferred removeBriefingFile would miss): the leftover file stays
// git-ignored until the next run overwrites and cleans it. Idempotent and
// best-effort — a no-op when workdir is not a standard git worktree.
func gitExcludeBriefing(workdir, relPath string) {
	if workdir == "" || relPath == "" {
		return
	}
	// A linked worktree/submodule stores .git as a file pointing elsewhere;
	// resolving its info/exclude is nontrivial, so only handle a real .git dir.
	gitDir := filepath.Join(workdir, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return
	}
	infoDir := filepath.Join(gitDir, "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return
	}
	excludePath := filepath.Join(infoDir, "exclude")
	rel := filepath.ToSlash(relPath)
	if data, err := os.ReadFile(excludePath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == rel {
				return // already excluded
			}
		}
	}
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(rel + "\n")
}
