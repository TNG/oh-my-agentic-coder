package vm

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
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

// versionRE accepts a version word such as "27.3.1" or "1.47", and no
// control or invalid bytes.
var versionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+~-]*$`)

// dockerCheckTimeout bounds one Docker check, so that a hanging SSH
// connection cannot use up a caller's whole wait.
const dockerCheckTimeout = 10 * time.Second

// ErrSSHLoginRejected means the VM refused omac's SSH key or presented a host
// key that differs from the recorded one. Waiting does not change either.
var ErrSSHLoginRejected = errors.New("the VM rejected omac's SSH login, or a program in the VM printed that refusal; " +
	"if it persists, run `omac vm reset`, which recreates the VM and deletes its images")

// dockerVersion asks the VM's Docker for its version over the agent's ssh.
func (m *Manager) dockerVersion(ctx context.Context, st *State) (DockerVersion, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerCheckTimeout)
	defer cancel()
	out := cappedBuffer{max: vmOutputLimit}
	if err := m.ssh(ctx, m.agentTarget(st), nil, &out, dockerVersionCmd); err != nil {
		if sshLoginRejected(err) {
			return DockerVersion{}, fmt.Errorf("%w: %w", ErrSSHLoginRejected, err)
		}
		return DockerVersion{}, err
	}
	fields := strings.Fields(out.String())
	if len(fields) != 2 || !versionRE.MatchString(fields[0]) || !versionRE.MatchString(fields[1]) {
		return DockerVersion{}, fmt.Errorf("docker version printed %q, want \"<api version> <version>\"", out.String())
	}
	return DockerVersion{APIVersion: fields[0], Version: fields[1]}, nil
}

// waitDocker polls dockerVersion until Docker answers or timeout passes.
func (m *Manager) waitDocker(ctx context.Context, st *State, timeout time.Duration) error {
	if m.Deps.ProbeInterval <= 0 {
		return fmt.Errorf("omac vm: probe interval must be positive, got %v", m.Deps.ProbeInterval)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(m.Deps.ProbeInterval)
	defer ticker.Stop()
	for {
		_, err := m.dockerVersion(ctx, st)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrSSHLoginRejected) {
			return fmt.Errorf("omac vm: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("omac vm: docker in the VM not ready (last error: %w): %w", err, ctx.Err())
		case <-ticker.C:
		}
	}
}

// sshLoginRejected matches the messages OpenSSH prints for a refused key and
// for a changed host key; m.ssh folds them into the error. Only exit status
// 255 counts, but ssh also passes on a remote command's 255: a command in
// the VM, for example a docker function in the agent's ~/.bashrc, can print
// the same text, exit 255 and still match.
func sshLoginRejected(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 255 {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Permission denied (") || strings.Contains(msg, "Host key verification failed")
}
