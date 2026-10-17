package session

import (
	"path/filepath"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// OpenCodeDBPath exposes opencode's session store db (plus its sqlite
// sidecar files, which carry writes when the db uses WAL mode) so
// host-side watchers can observe store activity without parsing. See
// opencodeDBPath for the resolution rules.
func OpenCodeDBPath() string { return opencodeDBPath() }

// ClaudeProjectDir returns the claude projects subdirectory that holds
// workdir's session files, honoring CLAUDE_CONFIG_DIR. The encoded
// directory name is a lossy hint (distinct paths can collide); callers
// that need authoritative membership must still check each file's
// embedded cwd, as listClaude does.
func ClaudeProjectDir(h config.Harness, workdir string) string {
	root := claudeProjectsRoot(h)
	if root == "" {
		return ""
	}
	return filepath.Join(root, encodeProjectDir(workdir))
}
