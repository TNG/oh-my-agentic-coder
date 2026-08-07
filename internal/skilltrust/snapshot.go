package skilltrust

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Snapshotting freezes a skill's on-disk content at approval time into a
// host-only directory the sandbox cannot write, and omac spawns the sidecar
// from that copy. This makes the executed bytes exactly the approved bytes:
// it closes the gap that the bundle hash (config.BundleHash) leaves open —
// dependency/artifact subtrees (node_modules, .venv, dist, …) and symlinks
// are excluded from the hash, so an agent could otherwise rewrite code under
// them after approval without changing the hash. A snapshot also removes the
// check-then-exec TOCTOU: the gate verifies a hash, but exec then runs the
// frozen copy, not the still-mutable workdir.
//
// Snapshots live under <store>/skills/<name>/<hash> (content-addressed), so
// identical content is stored once and re-approval is idempotent.

// snapshotRel returns the store-relative snapshot path for (name, hash).
// The bundle hash's "sha256:" prefix is stripped to a bare hex leaf.
func snapshotRel(name, bundleHash string) string {
	return filepath.Join("skills", name, strings.TrimPrefix(bundleHash, "sha256:"))
}

// SnapshotPath returns the absolute snapshot directory for (name, hash) and
// whether it currently exists on disk.
func SnapshotPath(name, bundleHash string) (string, bool) {
	d := dir()
	if d == "" {
		return "", false
	}
	p := filepath.Join(d, snapshotRel(name, bundleHash))
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return p, true
	}
	return p, false
}

// Snapshot freezes srcDir into the snapshot location for (name, hash) and
// returns the snapshot directory. It is idempotent and content-addressed: an
// existing snapshot for the same (name, hash) is returned unchanged (the
// content is identical by construction). The copy is staged in a temp dir and
// atomically renamed into place, so a snapshot directory is never partial.
func Snapshot(name, bundleHash, srcDir string) (string, error) {
	d := dir()
	if d == "" {
		return "", errNoGlobalDir
	}
	dst := filepath.Join(d, snapshotRel(name, bundleHash))
	if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
		return dst, nil
	}
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("snapshot: mkdir: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("snapshot: temp: %w", err)
	}
	if err := copyTree(srcDir, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("snapshot: copy: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.RemoveAll(tmp)
		// A concurrent writer may have won the race; accept its result.
		if fi, e2 := os.Stat(dst); e2 == nil && fi.IsDir() {
			return dst, nil
		}
		return "", fmt.Errorf("snapshot: rename: %w", err)
	}
	return dst, nil
}

// removeSnapshot deletes the snapshot for (name, hash) and prunes the now-empty
// per-name directory. Best-effort (used by Revoke).
func removeSnapshot(name, bundleHash string) {
	d := dir()
	if d == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(d, snapshotRel(name, bundleHash)))
	_ = os.Remove(filepath.Join(d, "skills", name)) // succeeds only if empty
}

// copyTree recursively copies src into dst, which must already exist. It
// captures a self-contained, immutable image of a skill:
//   - directories are recreated (VCS metadata under .git is skipped),
//   - regular files are copied with their mode bits (execute bits preserved),
//   - symlinks are flattened to a copy of their target's CONTENT when the
//     target resolves to a regular file (so a later repoint of the link cannot
//     change what runs); links to directories or unresolvable targets are
//     skipped.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(target, 0o700)
		}

		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Flatten to the target's content if it is a regular file.
			resolved, rerr := filepath.EvalSymlinks(p)
			if rerr != nil {
				return nil // dangling / unresolvable: drop it
			}
			ri, rierr := os.Stat(resolved)
			if rierr != nil || !ri.Mode().IsRegular() {
				return nil // non-regular target: drop it
			}
			return copyFile(resolved, target, ri.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return nil // sockets, devices, fifos: nothing to run
		}
		return copyFile(p, target, info.Mode().Perm())
	})
}

// copyFile copies a single regular file, creating parent dirs as needed and
// applying perm (so an executable entry-point stays executable).
func copyFile(src, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
