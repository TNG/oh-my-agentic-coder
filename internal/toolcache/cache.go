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

func PreparePersistent(domain Domain, path string) (*Scope, error) {
	scope, err := DescribePersistent(domain, path)
	if err != nil {
		return nil, err
	}
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

func Environment(dir string, mode Mode) map[string]string {
	return map[string]string{
		"XDG_CACHE_HOME":   filepath.Join(dir, "xdg"),
		"GOCACHE":          filepath.Join(dir, "go-build"),
		"GOMODCACHE":       filepath.Join(dir, "go-mod"),
		"NPM_CONFIG_CACHE": filepath.Join(dir, "npm"),
		"PIP_CACHE_DIR":    filepath.Join(dir, "pip"),
		"CARGO_HOME":       filepath.Join(dir, "cargo"),
		"OMAC_CACHE_DIR":   dir,
		"OMAC_CACHE_MODE":  string(mode),
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
	return os.Chmod(path, 0o700)
}
