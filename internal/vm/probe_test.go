package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// answerDocker makes the manager's ssh print out for every command, or fail
// with err.
func answerDocker(m *Manager, out string, err error) *[]string {
	var remotes []string
	m.Deps.SSH = func(_ context.Context, _ SSHTarget, _ io.Reader, stdout, _ io.Writer, remote string) error {
		remotes = append(remotes, remote)
		if err != nil {
			return err
		}
		fmt.Fprint(stdout, out)
		return nil
	}
	return &remotes
}

func TestDockerVersionParsesOutput(t *testing.T) {
	m, _, _ := newTestManager(t)
	remotes := answerDocker(m, "1.47 27.3.1\n", nil)
	v, err := m.dockerVersion(context.Background(), &State{SSHPort: 42022})
	if err != nil {
		t.Fatal(err)
	}
	if v != (DockerVersion{APIVersion: "1.47", Version: "27.3.1"}) {
		t.Errorf("version = %+v", v)
	}
	if len(*remotes) != 1 || (*remotes)[0] != dockerVersionCmd {
		t.Errorf("ssh commands = %q, want only %q", *remotes, dockerVersionCmd)
	}
	if !strings.Contains(dockerVersionCmd, "unix:///run/user/$(id -u)/docker.sock") || strings.Contains(dockerVersionCmd, "tcp://") {
		t.Errorf("dockerVersionCmd = %q, want the rootless Unix socket", dockerVersionCmd)
	}
}

func TestDockerVersionFailures(t *testing.T) {
	cases := map[string]struct {
		out string
		err error
	}{
		"ssh fails":    {err: errors.New("exit status 1: Cannot connect to the Docker daemon")},
		"empty output": {out: "\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m, _, _ := newTestManager(t)
			answerDocker(m, c.out, c.err)
			if v, err := m.dockerVersion(context.Background(), &State{SSHPort: 42022}); err == nil {
				t.Fatalf("dockerVersion = %+v, want an error", v)
			}
		})
	}
}

func TestWaitDockerRetriesUntilDockerAnswers(t *testing.T) {
	m, _, _ := newTestManager(t)
	calls := 0
	m.Deps.SSH = func(_ context.Context, _ SSHTarget, _ io.Reader, stdout, _ io.Writer, _ string) error {
		calls++
		if calls <= 2 {
			return errors.New("exit status 1")
		}
		fmt.Fprint(stdout, "1.47 27.3.1\n")
		return nil
	}
	if err := m.waitDocker(context.Background(), &State{SSHPort: 42022}, time.Second); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestWaitDockerTimesOut(t *testing.T) {
	m, _, _ := newTestManager(t)
	answerDocker(m, "", errors.New("exit status 1"))
	err := m.waitDocker(context.Background(), &State{SSHPort: 42022}, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.HasPrefix(err.Error(), "omac vm: docker in the VM not ready") {
		t.Fatalf("err = %v, want the not-ready DeadlineExceeded", err)
	}
}
