package vm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// UserConfig is ~/.config/omac/vm.yaml. Every field is optional.
type UserConfig struct {
	CPUs     int      `yaml:"cpus"`
	Memory   string   `yaml:"memory"`
	Disk     string   `yaml:"disk"`
	JDKs     []string `yaml:"jdks"`
	Packages []string `yaml:"packages"`
	// Setup is a shell script run as root while the VM is online after
	// packages and JDKs are installed. It must tolerate running again.
	Setup string `yaml:"setup"`
}

// Resources are the VM sizes handed to Lima.
type Resources struct {
	CPUs   int
	Memory string
	Disk   string
}

// HostResources is what DetectHost could find out; zero means unknown.
type HostResources struct {
	CPUs        int
	MemoryBytes uint64
}

const (
	minCPUs     = 4
	minMemGiB   = 8
	maxMemGiB   = 12
	defaultDisk = "60GiB"
)

// LoadUserConfig reads vm.yaml. A missing file or empty path yields the
// zero config. Unknown keys are an error so a typo does not silently fall
// back to defaults.
func LoadUserConfig(path string) (UserConfig, error) {
	var cfg UserConfig
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return UserConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.CPUs < 0 {
		return UserConfig{}, fmt.Errorf("%s: cpus must be positive, got %d", path, cfg.CPUs)
	}
	return cfg, nil
}

// ResolveResources applies the spec defaults: half the host cores (min 4),
// a quarter of host RAM clamped to 8-12GiB, 60GiB disk; explicit values win.
func ResolveResources(cfg UserConfig, host HostResources) Resources {
	res := Resources{CPUs: cfg.CPUs, Memory: cfg.Memory, Disk: cfg.Disk}
	if res.CPUs <= 0 {
		res.CPUs = max(host.CPUs/2, minCPUs)
	}
	if res.Memory == "" {
		gib := int(host.MemoryBytes / (1 << 30) / 4)
		res.Memory = fmt.Sprintf("%dGiB", min(max(gib, minMemGiB), maxMemGiB))
	}
	if res.Disk == "" {
		res.Disk = defaultDisk
	}
	return res
}
