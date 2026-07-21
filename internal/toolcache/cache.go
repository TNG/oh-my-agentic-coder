package toolcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type Mode string

const (
	ModePersistent Mode = "persistent"
	ModeEphemeral  Mode = "ephemeral"
)

type Domain string

const (
	DomainWorkdir Domain = "workdir"
	DomainServe   Domain = "serve"
	// DomainHarness keys a cache scope by harness identity, not workdir. The
	// harness's own XDG cache holds config-declared plugins — user state, like
	// the already-global ~/.config/<harness> — so sharing it across workdirs
	// avoids a per-project registry re-fetch without weakening build-cache
	// isolation (which stays per-workdir).
	DomainHarness Domain = "harness"
)

type Scope struct {
	Domain        Domain
	Mode          Mode
	CanonicalPath string
	Identity      string
	Digest        string
	Dir           string
	lock          *os.File
}

func DescribePersistent(domain Domain, path string) (Scope, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return Scope{}, err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Scope{}, err
	}

	identity := "v1:" + string(domain) + ":" + canonical
	digest := sha256.Sum256([]byte(identity))
	home, err := os.UserHomeDir()
	if err != nil {
		return Scope{}, err
	}

	return Scope{
		Domain:        domain,
		Mode:          ModePersistent,
		CanonicalPath: canonical,
		Identity:      identity,
		Digest:        hex.EncodeToString(digest[:]),
		Dir:           filepath.Join(home, ".cache", "omac", hex.EncodeToString(digest[:])),
	}, nil
}

// DescribeHarness returns the persistent, cross-workdir cache scope for a
// harness, keyed by harness name instead of a workdir path. It does not touch
// the filesystem. The digest inputs never include a path, so the same harness
// resolves to the same scope from every directory.
func DescribeHarness(harness string) (Scope, error) {
	if harness == "" {
		return Scope{}, errors.New("harness name is empty")
	}
	identity := "v1:harness:" + harness
	digest := sha256.Sum256([]byte(identity))
	home, err := os.UserHomeDir()
	if err != nil {
		return Scope{}, err
	}
	return Scope{
		Domain:   DomainHarness,
		Mode:     ModePersistent,
		Identity: identity,
		Digest:   hex.EncodeToString(digest[:]),
		Dir:      filepath.Join(home, ".cache", "omac", hex.EncodeToString(digest[:])),
	}, nil
}

// PrepareHarness creates and locks the harness-scoped persistent cache,
// mirroring PreparePersistent for the workdir/serve domains.
func PrepareHarness(harness string) (*Scope, error) {
	scope, err := DescribeHarness(harness)
	if err != nil {
		return nil, err
	}
	return prepareScope(scope)
}

func PreparePersistent(domain Domain, path string) (*Scope, error) {
	scope, err := DescribePersistent(domain, path)
	if err != nil {
		return nil, err
	}
	return prepareScope(scope)
}

func prepareScope(scope Scope) (*Scope, error) {
	lock, err := acquireLock(filepath.Dir(scope.Dir), scope.Digest, syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(scope.Dir); err != nil {
		_ = releaseLock(lock)
		return nil, err
	}
	scope.lock = lock
	return &scope, nil
}

func PrepareEphemeral(sandboxTmp string) (*Scope, error) {
	dir := filepath.Join(sandboxTmp, "cache")
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	return &Scope{
		Mode: ModeEphemeral,
		Dir:  dir,
	}, nil
}

// Environment returns the cache redirects for a single-scope launch where the
// harness XDG cache and the tool-specific build caches share one directory
// (ephemeral mode, and diagnostics such as provenance).
func Environment(dir string, mode Mode) map[string]string {
	return EnvironmentSplit(dir, dir, mode)
}

// EnvironmentSplit returns the cache redirects when the harness's own XDG
// cache lives in a different scope from the build caches. buildDir backs the
// per-workdir build caches (Go/npm/pip/Cargo, OMAC_CACHE_DIR); xdgDir backs
// XDG_CACHE_HOME (OMAC_XDG_CACHE_DIR), which the sandbox re-exec grant-checks
// independently.
func EnvironmentSplit(buildDir, xdgDir string, mode Mode) map[string]string {
	return map[string]string{
		"XDG_CACHE_HOME":     filepath.Join(xdgDir, "xdg"),
		"GOCACHE":            filepath.Join(buildDir, "go-build"),
		"GOMODCACHE":         filepath.Join(buildDir, "go-mod"),
		"NPM_CONFIG_CACHE":   filepath.Join(buildDir, "npm"),
		"PIP_CACHE_DIR":      filepath.Join(buildDir, "pip"),
		"CARGO_HOME":         filepath.Join(buildDir, "cargo"),
		"OMAC_CACHE_DIR":     buildDir,
		"OMAC_XDG_CACHE_DIR": xdgDir,
		"OMAC_CACHE_MODE":    string(mode),
	}
}

func (s *Scope) Close() error {
	if s == nil || s.lock == nil {
		return nil
	}
	err := releaseLock(s.lock)
	s.lock = nil
	return err
}

func ensurePrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
		if err != nil {
			return err
		}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("cache directory %q is a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("cache path %q is not a directory", path)
	}
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open cache directory for chmod: %w", err)
	}
	defer dir.Close()
	if err := dir.Chmod(0o700); err != nil {
		return fmt.Errorf("chmod cache directory: %w", err)
	}
	return nil
}
