//go:build linux

package gitservice

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func supportedPlatform() error {
	for _, path := range []string{"/usr/bin/git", "/usr/bin/bwrap"} {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		var magic [4]byte
		_, err = io.ReadFull(f, magic[:])
		f.Close()
		if err != nil || string(magic[:]) != "\x7fELF" {
			return fmt.Errorf("prototype requires a trusted ELF executable at %s", path)
		}
	}
	return nil
}

func multipleLinks(info os.FileInfo) bool { return info.Sys().(*syscall.Stat_t).Nlink != 1 }

func lockRepository(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("repository already has a Git service: %w", err)
	}
	return f, nil
}

func (s *Service) workerArgv(metadataWrite, projectWrite bool, args []string) ([]string, error) {
	argv := []string{"--die-with-parent", "--new-session", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-net", "--cap-drop", "ALL", "--clearenv"}
	for _, path := range []string{"/usr", "/lib", "/lib64"} {
		if _, err := os.Stat(path); err == nil {
			argv = append(argv, "--ro-bind", path, path)
		}
	}
	argv = append(argv, "--symlink", "usr/bin", "/bin", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--dir", "/tmp/home")
	flag := "--ro-bind"
	if projectWrite {
		flag = "--bind"
	}
	argv = append(argv, flag, s.root, "/repo")
	for _, parent := range s.deniedParents {
		rel, _ := filepath.Rel(s.root, parent)
		argv = append(argv, flag, parent, filepath.Join("/repo", rel))
	}
	flag = "--ro-bind"
	if metadataWrite {
		flag = "--bind"
	}
	argv = append(argv, flag, filepath.Join(s.root, ".git"), "/repo/.git", "--ro-bind", s.runtime, "/omac", "--ro-bind", filepath.Join(s.root, ".git", "config"), "/omac/source-config", "--ro-bind", filepath.Join(s.runtime, "config"), "/repo/.git/config")
	for _, path := range s.denied {
		if !within(path, s.root) {
			continue
		}
		if within(filepath.Join(s.root, ".git"), path) || within(path, filepath.Join(s.root, ".git")) {
			return nil, fmt.Errorf("Git metadata is denied")
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("denied path is a symlink: %s", path)
		}
		src := "/dev/null"
		if info.IsDir() {
			src = filepath.Join(s.runtime, "empty")
		}
		rel, _ := filepath.Rel(s.root, path)
		argv = append(argv, "--ro-bind", src, filepath.Join("/repo", rel))
	}
	for _, item := range [][2]string{{"PATH", "/usr/bin:/bin"}, {"HOME", "/tmp/home"}, {"XDG_CONFIG_HOME", "/tmp/home"}, {"LANG", "C.UTF-8"}, {"GIT_CONFIG_NOSYSTEM", "1"}, {"GIT_ATTR_NOSYSTEM", "1"}, {"GIT_CONFIG_GLOBAL", "/dev/null"}, {"GIT_TERMINAL_PROMPT", "0"}, {"GIT_OPTIONAL_LOCKS", "0"}, {"GIT_LITERAL_PATHSPECS", "1"}, {"GIT_NO_REPLACE_OBJECTS", "1"}, {"GIT_PAGER", "cat"}} {
		argv = append(argv, "--setenv", item[0], item[1])
	}
	argv = append(argv, "--chdir", "/repo", "--", "/usr/bin/git", "--no-pager", "--git-dir=/repo/.git", "--work-tree=/repo")
	return append(argv, args...), nil
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > maxOutput-b.buffer.Len() {
		b.overflow = true
		return 0, fmt.Errorf("Git output exceeds limit")
	}
	return b.buffer.Write(p)
}

func (s *Service) run(ctx context.Context, metadataWrite, projectWrite bool, input []byte, args ...string) ([]byte, error) {
	argv, err := s.workerArgv(metadataWrite, projectWrite, args)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/bwrap", argv...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	if stdout.overflow || stderr.overflow {
		return nil, fmt.Errorf("Git worker: Git output exceeds limit")
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.buffer.String())
		if len(msg) > 4096 {
			msg = msg[:4096]
		}
		return nil, fmt.Errorf("Git worker: %w: %s", err, msg)
	}
	return stdout.buffer.Bytes(), nil
}
