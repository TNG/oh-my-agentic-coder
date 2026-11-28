package opencodestate

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSecurityAgentWritableStateNotGrantSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")

	storageDir := filepath.Join(home, ".local", "share", "opencode", "storage", "project")
	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	inHome := filepath.Join(home, "Documents")
	if err := os.MkdirAll(inHome, 0o755); err != nil {
		t.Fatal(err)
	}

	control := securityOutOfHomeDir(t)
	records := []string{os.TempDir(), "/etc", inHome}
	for i, worktree := range records {
		writeProject(t, storageDir, fmt.Sprintf("planted-%d", i), worktree)
	}
	securityWriteProjectDB(t, home, records...)
	securityWriteDesktopProjects(t, home, control)

	worktrees, _, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(worktrees, control) {
		t.Fatalf("control: Desktop record %q was not returned: %v", control, worktrees)
	}
	for _, worktree := range records {
		if slices.Contains(worktrees, worktree) {
			t.Errorf("record from agent-writable OpenCode state was returned as a grant source: %q: %v", worktree, worktrees)
		}
	}
}

func TestSecurityWorktreeGrantExcludesSharedTemp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")

	base := securityOutOfHomeDir(t)
	control := securityOutOfHomeDir(t)

	worldWritable := filepath.Join(base, "world-writable")
	if err := os.Mkdir(worldWritable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(worldWritable, 0o777); err != nil {
		t.Fatal(err)
	}
	sticky := filepath.Join(base, "sticky")
	if err := os.Mkdir(sticky, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o1777); err != nil {
		t.Fatal(err)
	}

	var rejected []string
	seen := map[string]bool{}
	add := func(path string) {
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			rejected = append(rejected, path)
		}
	}
	for _, path := range []string{"/tmp", "/var/tmp", "/dev/shm", worldWritable, sticky} {
		add(path)
	}

	securityWriteDesktopProjects(t, home, append(rejected, control)...)

	worktrees, skipped, err := Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(worktrees, control) {
		t.Fatalf("control: trusted out-of-home record %q was not returned: %v", control, worktrees)
	}
	for _, worktree := range rejected {
		if slices.Contains(worktrees, worktree) {
			t.Errorf("shared-temp or writable directory %q was returned as a grant source: %v", worktree, worktrees)
		}
	}
	for _, worktree := range []string{worldWritable, sticky} {
		if !slices.Contains(skipped, SkippedWorktree{Path: worktree, Reason: SkipUntrusted}) {
			t.Errorf("writable directory %q was not rejected as untrusted: %v", worktree, skipped)
		}
	}
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

func securityPathWithin(root, path string) bool {
	if root == "" {
		return false
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
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

func securityWriteDesktopProjects(t *testing.T, home string, worktrees ...string) {
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
	dir := filepath.Join(home, "Library", "Application Support", "ai.opencode.desktop")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.global.dat"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func securityWriteProjectDB(t *testing.T, home string, worktrees ...string) {
	t.Helper()
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Logf("sqlite3 not available; skipping opencode.db fixture")
		return
	}
	db := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	var statement strings.Builder
	statement.WriteString("CREATE TABLE project (worktree TEXT);")
	for _, worktree := range worktrees {
		fmt.Fprintf(&statement, "INSERT INTO project (worktree) VALUES ('%s');", strings.ReplaceAll(worktree, "'", "''"))
	}
	if out, err := exec.Command(sqlite, db, statement.String()).CombinedOutput(); err != nil {
		t.Fatalf("sqlite3 fixture: %v: %s", err, out)
	}
}
