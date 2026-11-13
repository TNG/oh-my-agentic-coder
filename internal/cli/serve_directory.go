package cli

import (
	"fmt"
	"os"
	"path/filepath"
)

// pinRoots runs before accepting requests. Tests that construct a server
// directly also use it on first activation, before validating the candidate.
func (s *serveServer) pinRoots() error {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	if s.rootDirs == nil {
		s.rootDirs = map[string]*os.Root{}
	}
	for _, path := range s.roots {
		if s.rootDirs[path] != nil {
			continue
		}
		r, err := os.OpenRoot(path)
		if err != nil {
			return fmt.Errorf("open --root %s: %w", path, err)
		}
		s.rootDirs[path] = r
	}
	return nil
}

func (s *serveServer) openActivationRoot(name string) (string, *os.Root, error) {
	if err := s.pinRoots(); err != nil {
		return "", nil, err
	}
	fi, err := os.Lstat(name)
	if err != nil {
		return "", nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", nil, fmt.Errorf("refusing to activate %s: the final path component is a symbolic link", name)
	}
	if !fi.IsDir() {
		return "", nil, fmt.Errorf("not a directory: %s", name)
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", nil, err
	}
	if !s.dirAllowed(resolved) {
		return "", nil, fmt.Errorf("directory %s is not under any allowed --root", name)
	}
	var r *os.Root
	if len(s.roots) == 0 {
		r, err = os.OpenRoot(resolved)
	} else {
		// Open beneath the already-pinned policy root, not its pathname.
		// Root.OpenRoot rejects an escaping symlink even during replacement.
		for _, path := range s.roots {
			rel, rerr := filepath.Rel(path, resolved)
			if rerr != nil || startsWithDotDot(rel) || filepath.IsAbs(rel) {
				continue
			}
			s.actMu.Lock()
			parent := s.rootDirs[path]
			s.actMu.Unlock()
			r, err = parent.OpenRoot(rel)
			if err == nil {
				break
			}
		}
	}
	if err != nil {
		return "", nil, err
	}
	if r == nil {
		return "", nil, fmt.Errorf("directory %s has no opened policy root", name)
	}
	opened, err := r.Stat(".")
	if err != nil || !os.SameFile(fi, opened) {
		r.Close()
		return "", nil, fmt.Errorf("directory %s changed during admission", name)
	}
	return resolved, r, nil
}

// sameDirectoryName is a fail-fast check, not the filesystem boundary. Every
// subsequent project operation must still use root, even if this check passed.
func sameDirectoryName(name string, root *os.Root) bool {
	fi, err := os.Lstat(name)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	opened, err := root.Stat(".")
	return err == nil && os.SameFile(fi, opened)
}

func (s *serveServer) rememberDirName(name string, d *dirState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirNames == nil {
		s.dirNames = map[string]*dirState{}
	}
	s.dirNames[name] = d
	s.dirNames[d.Dir] = d
}

// closeDirectoryRoots is called after the control server stops accepting work.
func (s *serveServer) closeDirectoryRoots() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actMu.Lock()
	defer s.actMu.Unlock()
	for _, d := range s.dirs {
		if d.Root != nil {
			d.Root.Close()
		}
	}
	for _, r := range s.rootDirs {
		r.Close()
	}
}
