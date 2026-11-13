package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DockerVersion is the part of `docker version` omac reports.
type DockerVersion struct {
	APIVersion string
	Version    string
}

// dockerVersionCmd prints "<api version> <version>" of the VM's dockerd and
// fails while dockerd does not answer on its socket.
var dockerVersionCmd = dockerCmd("version --format '{{.Server.APIVersion}} {{.Server.Version}}'")

// dockerVersion asks the VM's Docker for its version over the agent's ssh.
func (m *Manager) dockerVersion(ctx context.Context, st *State) (DockerVersion, error) {
	var out bytes.Buffer
	if err := m.ssh(ctx, m.agentTarget(st), nil, &out, dockerVersionCmd); err != nil {
		return DockerVersion{}, err
	}
	api, version, _ := strings.Cut(strings.TrimSpace(out.String()), " ")
	if api == "" {
		return DockerVersion{}, errors.New("docker version printed no API version")
	}
	return DockerVersion{APIVersion: api, Version: version}, nil
}

// waitDocker polls dockerVersion until Docker answers or timeout passes.
func (m *Manager) waitDocker(ctx context.Context, st *State, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(m.Deps.ProbeInterval)
	defer ticker.Stop()
	for {
		_, err := m.dockerVersion(ctx, st)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("omac vm: docker in the VM not ready (last error: %v): %w", err, ctx.Err())
		case <-ticker.C:
		}
	}
}
