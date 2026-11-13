package registry

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"syscall"
)

// LoadRoot reads the registry through the admitted project directory.
func LoadRoot(root *os.Root) (*Registry, error) {
	raw, err := root.ReadFile(".opencode/sidecar.json")
	return parseRegistry(raw, err)
}

// WithRootLock is WithLock without reopening the project's absolute pathname.
func WithRootLock(root *os.Root, fn func() error) error {
	if err := root.MkdirAll(".opencode", 0o700); err != nil {
		return fmt.Errorf("ensure registry dir: %w", err)
	}
	f, err := root.OpenFile(".opencode/sidecar.json.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// SaveRoot atomically saves within root; the caller must hold WithRootLock.
func SaveRoot(root *os.Root, r *Registry) error {
	if r.Version == 0 {
		r.Version = SchemaVersion
	}
	sort.SliceStable(r.Registered, func(i, j int) bool { return r.Registered[i].Name < r.Registered[j].Name })
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	if err := root.MkdirAll(".opencode", 0o700); err != nil {
		return fmt.Errorf("ensure registry dir: %w", err)
	}
	name := ".opencode/sidecar.json.tmp-" + rand.Text()
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	defer root.Remove(name)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := root.Rename(name, ".opencode/sidecar.json"); err != nil {
		return fmt.Errorf("rename registry: %w", err)
	}
	return nil
}
