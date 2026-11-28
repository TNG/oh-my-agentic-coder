package gitservice

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func (s *Service) validateRepository() error {
	for _, item := range []struct {
		path string
		info os.FileInfo
	}{{s.root, s.rootInfo}, {filepath.Join(s.root, ".git"), s.gitInfo}} {
		info, err := os.Lstat(item.path)
		if err != nil || !info.IsDir() || !os.SameFile(info, item.info) {
			return fmt.Errorf("registered repository was replaced: %s", item.path)
		}
	}
	gitdir := filepath.Join(s.root, ".git")
	for _, name := range []string{"commondir", "modules", "worktrees", "shallow", "objects/info/alternates", "objects/info/http-alternates", "info/sparse-checkout", "rebase-merge", "rebase-apply", "sequencer", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG"} {
		if _, err := os.Lstat(filepath.Join(gitdir, name)); !os.IsNotExist(err) {
			return fmt.Errorf("unsupported repository state: .git/%s", name)
		}
	}
	entries := 0
	return filepath.WalkDir(gitdir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if entries > maxEntries {
			return fmt.Errorf("Git metadata exceeds %d entries", maxEntries)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() || multipleLinks(info) {
			return fmt.Errorf("Git metadata must contain only directories and single-link regular files: %s", path)
		}
		return nil
	})
}

func (s *Service) validatePath(path string, targetTree bool) error {
	if path == "" || len(path) > 4096 || filepath.IsAbs(path) || filepath.Clean(path) != path || path == "." || strings.ContainsAny(path, "\x00\\\r\n") {
		return fmt.Errorf("expected an explicit literal repository-relative file path: %q", path)
	}
	parts := strings.Split(path, "/")
	for _, p := range parts {
		if p == ".." || strings.EqualFold(p, ".git") || strings.EqualFold(p, ".gitmodules") {
			return fmt.Errorf("metadata or submodule path is not supported: %q", path)
		}
	}
	abs := filepath.Join(s.root, path)
	for _, deny := range s.denied {
		if within(abs, deny) {
			return fmt.Errorf("path is denied by the sandbox: %q", path)
		}
	}
	if targetTree {
		return nil
	}
	current := s.root
	for i, part := range parts {
		if i > 0 {
			if _, err := os.Lstat(filepath.Join(current, ".git")); !os.IsNotExist(err) {
				return fmt.Errorf("nested repository path is not supported: %q", path)
			}
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if i != len(parts)-1 {
			if !info.IsDir() {
				return fmt.Errorf("path traverses a non-directory: %q", path)
			}
		} else if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("stage explicit files, not directories or special files: %q", path)
		}
	}
	return nil
}
