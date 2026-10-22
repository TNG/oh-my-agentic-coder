package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// ErrLimaMissing reports that limactl is not installed.
var ErrLimaMissing = errors.New("limactl not found on $PATH")

// Lima instance states as reported by `limactl list --json`.
const (
	StatusRunning = "Running"
	StatusStopped = "Stopped"
)

// FindLimactl resolves limactl via lookPath (exec.LookPath in production).
func FindLimactl(lookPath func(string) (string, error)) (string, error) {
	p, err := lookPath("limactl")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrLimaMissing, err)
	}
	return p, nil
}

// LimaCtl runs limactl with LIMA_HOME set to limaHome. Its stderr goes to
// os.Stderr; a nil stdout discards the output.
type LimaCtl func(ctx context.Context, limaHome string, stdout io.Writer, args ...string) error

// DefaultLimaCtl runs the limactl binary found on $PATH.
func DefaultLimaCtl(ctx context.Context, limaHome string, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "limactl", args...)
	cmd.Env = append(os.Environ(), "LIMA_HOME="+limaHome)
	if stdout == nil {
		stdout = io.Discard
	}
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Instance is the subset of `limactl list --json` that omac reads.
type Instance struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	SSHLocalPort int    `json:"sshLocalPort"`
}

// Inspect reports the omac instance, or nil when it does not exist.
func Inspect(ctx context.Context, run LimaCtl, limaHome string) (*Instance, error) {
	var out bytes.Buffer
	// No name argument: `limactl list --json omac` exits 1 when omac does not
	// exist, while the unfiltered list exits 0 and prints one JSON object per
	// instance and line (nothing for an empty LIMA_HOME).
	if err := run(ctx, limaHome, &out, "list", "--json"); err != nil {
		return nil, fmt.Errorf("omac vm: limactl list: %w", err)
	}
	for line := range bytes.Lines(out.Bytes()) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var inst Instance
		if err := json.Unmarshal(line, &inst); err != nil {
			return nil, fmt.Errorf("omac vm: parse limactl list output: %w", err)
		}
		if inst.Name == InstanceName {
			return &inst, nil
		}
	}
	return nil, nil
}
