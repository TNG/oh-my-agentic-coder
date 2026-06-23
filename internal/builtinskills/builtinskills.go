// Package builtinskills ships skill bundles embedded in the omac binary and
// materializes them onto disk.
//
// Today the only built-in is omac-write-a-skill, a guidance-only skill (a
// SKILL.md, no omac.yaml). omac deliberately ignores SKILL.md-only directories
// (see internal/skillsource), so these bundles are NOT registered or activated
// by omac; they are placed into a harness's own native skills directory and
// surfaced by that harness's loader directly. Provisioning is therefore pure
// file placement — see `omac setup`.
//
// The bundles are embedded so `omac setup` works in any folder, independent of
// the source tree the binary was built from, with no network access. Each
// bundle carries an `omac-builtin: "true"` marker in its SKILL.md frontmatter
// so re-provisioning can tell an omac-owned copy (safe to refresh) from a
// user's or third party's same-named directory (left untouched).
package builtinskills

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// assetsFS holds every built-in bundle under assets/<skill-name>/...
//
//go:embed all:assets
var assetsFS embed.FS

// assetsRoot is the embedded directory that holds the bundle subdirectories.
const assetsRoot = "assets"

// MarkerKey is the SKILL.md frontmatter metadata key that identifies a bundle
// as omac-owned. Its presence with a truthy value means `omac setup` may
// refresh the directory; its absence means the directory is foreign and must
// not be overwritten.
const MarkerKey = "omac-builtin"

// Names returns the built-in skill names (the bundle directory names), sorted.
func Names() []string {
	entries, err := assetsFS.ReadDir(assetsRoot)
	if err != nil {
		// The assets tree is embedded at build time; a read failure is a
		// programming error, not a runtime condition.
		panic("builtinskills: read embedded assets: " + err.Error())
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// bundleFS returns an fs.FS rooted at the named bundle's directory.
func bundleFS(name string) (fs.FS, error) {
	sub, err := fs.Sub(assetsFS, filepath.ToSlash(filepath.Join(assetsRoot, name)))
	if err != nil {
		return nil, fmt.Errorf("builtinskills: no bundle %q: %w", name, err)
	}
	// Probe so an unknown name fails here rather than at first walk.
	if _, err := fs.Stat(sub, "SKILL.md"); err != nil {
		return nil, fmt.Errorf("builtinskills: bundle %q has no SKILL.md: %w", name, err)
	}
	return sub, nil
}

// Status describes what Materialize did (or would do) for one bundle.
type Status string

const (
	// StatusCreated: the target did not exist and was written fresh.
	StatusCreated Status = "created"
	// StatusUpdated: an omac-owned target existed with different content and
	// was refreshed to the embedded version.
	StatusUpdated Status = "updated"
	// StatusUnchanged: the target already matched the embedded version.
	StatusUnchanged Status = "unchanged"
	// StatusForeign: a same-named directory exists without the omac-builtin
	// marker; it was left untouched (no write).
	StatusForeign Status = "foreign"
)

// Result reports the outcome of materializing one bundle into one location.
type Result struct {
	Skill  string
	Dir    string // absolute target bundle directory
	Status Status
}

// State describes a bundle's on-disk state relative to the embedded version,
// without modifying anything. Used by read-only callers like `omac doctor`.
type State string

const (
	// StateMissing: no directory for the bundle exists.
	StateMissing State = "missing"
	// StateCurrent: an omac-owned bundle exists and matches the embedded version.
	StateCurrent State = "current"
	// StateStale: an omac-owned bundle exists but differs from the embedded
	// version (e.g. omac was upgraded but `omac setup` not re-run).
	StateStale State = "stale"
	// StateForeign: a same-named directory exists without the omac-builtin marker.
	StateForeign State = "foreign"
)

// Check reports the on-disk state of bundle `name` under skillsParentDir
// without writing anything.
func Check(name, skillsParentDir string) (State, error) {
	src, err := bundleFS(name)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(skillsParentDir, name)
	existed, err := isDir(dir)
	if err != nil {
		return "", err
	}
	if !existed {
		return StateMissing, nil
	}
	owned, err := dirHasMarker(dir)
	if err != nil {
		return "", err
	}
	if !owned {
		return StateForeign, nil
	}
	identical, err := treeMatches(src, dir)
	if err != nil {
		return "", err
	}
	if identical {
		return StateCurrent, nil
	}
	return StateStale, nil
}

// Materialize writes the built-in skill `name` into skillsParentDir/<name>.
//
// It is marker-guarded: if the target directory already exists and is NOT
// omac-owned (its SKILL.md lacks the omac-builtin marker), it is left untouched
// and Status is StatusForeign — unless force is true, in which case it is
// overwritten. An omac-owned (or absent) target is written/refreshed; an
// already-current target is reported StatusUnchanged with no write.
//
// File writes are per-file atomic (temp file + rename). Directories are created
// 0755 and files 0644 (embedded files carry no mode bits; the only built-in is
// guidance-only text, so no executable bits are needed).
func Materialize(name, skillsParentDir string, force bool) (Result, error) {
	src, err := bundleFS(name)
	if err != nil {
		return Result{}, err
	}
	dir := filepath.Join(skillsParentDir, name)
	res := Result{Skill: name, Dir: dir}

	existed, err := isDir(dir)
	if err != nil {
		return res, err
	}
	if existed && !force {
		owned, err := dirHasMarker(dir)
		if err != nil {
			return res, err
		}
		if !owned {
			res.Status = StatusForeign
			return res, nil
		}
	}

	identical, err := treeMatches(src, dir)
	if err != nil {
		return res, err
	}
	if identical {
		res.Status = StatusUnchanged
		return res, nil
	}

	if err := writeTree(src, dir); err != nil {
		return res, err
	}
	if existed {
		res.Status = StatusUpdated
	} else {
		res.Status = StatusCreated
	}
	return res, nil
}

// isDir reports whether path is an existing directory.
func isDir(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.IsDir(), nil
}

var markerRe = regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(MarkerKey) + `:\s*"?(?i:true)"?\s*$`)

// dirHasMarker reports whether the SKILL.md in dir carries a truthy
// omac-builtin marker in its YAML frontmatter.
func dirHasMarker(dir string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	fm, ok := frontmatter(b)
	if !ok {
		return false, nil
	}
	return markerRe.Match(fm), nil
}

// frontmatter extracts the bytes of the leading YAML frontmatter block
// (between the first two `---` lines). ok is false when no frontmatter block
// is present.
func frontmatter(b []byte) ([]byte, bool) {
	// Must start with a "---" line (allow a leading BOM/whitespace-free start).
	if !bytes.HasPrefix(b, []byte("---\n")) && !bytes.HasPrefix(b, []byte("---\r\n")) {
		return nil, false
	}
	rest := b[bytes.IndexByte(b, '\n')+1:]
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return nil, false
	}
	return rest[:end], true
}

// treeMatches reports whether every file in the embedded bundle exists on disk
// under dir with byte-identical content. Extra files on disk are ignored (a
// user may have added notes); the check is "is the embedded content present and
// current", which is what idempotent refresh cares about.
func treeMatches(src fs.FS, dir string) (bool, error) {
	match := true
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		want, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			if os.IsNotExist(err) {
				match = false
				return fs.SkipAll
			}
			return err
		}
		if !bytes.Equal(want, got) {
			match = false
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return match, nil
}

// writeTree writes every file in the embedded bundle into dir, creating parent
// directories as needed. Each file is written atomically (temp + rename).
func writeTree(src fs.FS, dir string) error {
	return fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return atomicWrite(target, data)
	})
}

// atomicWrite writes data to path via a temp file in the same directory
// followed by rename, so a concurrent reader never sees a partial file.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}
