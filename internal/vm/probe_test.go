package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
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
}

// The SSH port is now the only way omac reaches Docker in the VM, and a
// non-interactive ssh command does not read DOCKER_HOST from ~/.bashrc.
func TestDockerVersionCmdNamesTheSocket(t *testing.T) {
	want := "docker -H unix:///run/user/$(id -u)/docker.sock version --format '{{.Server.APIVersion}} {{.Server.Version}}'"
	if dockerVersionCmd != want {
		t.Errorf("dockerVersionCmd = %q, want %q", dockerVersionCmd, want)
	}
}

// Every --vm session controls the VM's agent user, so the stderr of a
// failed command must not reach the host terminal as escape sequences or
// as lines that look like omac's own.
func TestManagerSSHEscapesStderr(t *testing.T) {
	m, _, _ := newTestManager(t)
	m.Deps.SSH = func(_ context.Context, _ SSHTarget, _ io.Reader, _, stderr io.Writer, _ string) error {
		fmt.Fprint(stderr, "error\x1b]0;x\a\nomac vm: forged \x9b2J")
		return errors.New("exit status 1")
	}
	_, err := m.dockerVersion(context.Background(), &State{SSHPort: 42022})
	if err == nil {
		t.Fatal("dockerVersion succeeded, want an error")
	}
	if msg := err.Error(); !utf8.ValidString(msg) || strings.IndexFunc(msg, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		t.Errorf("error holds bytes a terminal could act on: %q", msg)
	}
}

// A session can make a failing command print without end; omac keeps only
// the start of it.
func TestManagerSSHLimitsOutput(t *testing.T) {
	big := strings.Repeat("x", 2<<20)
	for name, answer := range map[string]func(stdout, stderr io.Writer) error{
		"stdout": func(stdout, _ io.Writer) error { fmt.Fprint(stdout, big); return nil },
		"stderr": func(_, stderr io.Writer) error { fmt.Fprint(stderr, big); return errors.New("exit status 1") },
	} {
		t.Run(name, func(t *testing.T) {
			m, _, _ := newTestManager(t)
			m.Deps.SSH = func(_ context.Context, _ SSHTarget, _ io.Reader, stdout, stderr io.Writer, _ string) error {
				return answer(stdout, stderr)
			}
			_, err := m.dockerVersion(context.Background(), &State{SSHPort: 42022})
			if err == nil || len(err.Error()) > 2*vmOutputLimit || !strings.Contains(err.Error(), "[truncated]") {
				t.Fatalf("dockerVersion error has %d bytes, want an error of at most %d that says it was cut", len(fmt.Sprint(err)), 2*vmOutputLimit)
			}
		})
	}
}

// Text a remote command prints is not ssh's own refusal, however it reads.
func TestDockerVersionLoginRejectedOnlyFromSSH(t *testing.T) {
	m, _, _ := newTestManager(t)
	m.Deps.SSH = func(_ context.Context, _ SSHTarget, _ io.Reader, _, stderr io.Writer, _ string) error {
		fmt.Fprint(stderr, "Error response from daemon: Permission denied (x)")
		return exitErr(t, 1)
	}
	if _, err := m.dockerVersion(context.Background(), &State{SSHPort: 42022}); err == nil || errors.Is(err, ErrSSHLoginRejected) {
		t.Fatalf("dockerVersion = %v, want an error that is not ErrSSHLoginRejected", err)
	}
}

func TestDockerVersionFailures(t *testing.T) {
	cases := map[string]struct {
		out string
		err error
	}{
		"ssh fails":    {err: errors.New("exit status 1: Cannot connect to the Docker daemon")},
		"empty output": {out: "\n"},
		"extra lines":  {out: "1.47 27.3.1\nomac vm: injected\n"},
		"escape codes": {out: "1.47 27.3.1\x1b[1A\x1b[2K\n"},
		"one word":     {out: "1.47\n"},
		"three words":  {out: "1.47 27.3.1 x\n"},
		"invalid byte": {out: "1.47 27.3.1\x9b2J\n"},
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

func TestDockerVersionBoundsEachCheck(t *testing.T) {
	m, _, _ := newTestManager(t)
	var deadline time.Time
	m.Deps.SSH = func(ctx context.Context, _ SSHTarget, _ io.Reader, stdout, _ io.Writer, _ string) error {
		deadline, _ = ctx.Deadline()
		fmt.Fprint(stdout, "1.47 27.3.1\n")
		return nil
	}
	if _, err := m.dockerVersion(context.Background(), &State{SSHPort: 42022}); err != nil {
		t.Fatal(err)
	}
	if latest := time.Now().Add(dockerCheckTimeout); deadline.IsZero() || deadline.After(latest) {
		t.Errorf("ssh deadline = %v, want one no later than %v", deadline, latest)
	}
}

func TestWaitDockerStopsWhenSSHLoginIsRejected(t *testing.T) {
	for name, stderr := range map[string]string{
		"key rejected":     "omac-alice@127.0.0.1: Permission denied (publickey).",
		"host key changed": "Host key verification failed.",
	} {
		t.Run(name, func(t *testing.T) {
			m, _, _ := newTestManager(t)
			calls := 0
			m.Deps.SSH = func(_ context.Context, _ SSHTarget, _ io.Reader, _, errOut io.Writer, _ string) error {
				calls++
				fmt.Fprintln(errOut, stderr)
				return exitErr(t, 255)
			}
			err := m.waitDocker(context.Background(), &State{SSHPort: 42022}, time.Minute)
			if !errors.Is(err, ErrSSHLoginRejected) || !strings.Contains(err.Error(), "omac vm reset") {
				t.Fatalf("err = %v, want ErrSSHLoginRejected naming `omac vm reset`", err)
			}
			if calls != 1 {
				t.Errorf("ssh calls = %d, want 1", calls)
			}
		})
	}
}

func TestWaitDockerRejectsNonPositiveInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		t.Run(interval.String(), func(t *testing.T) {
			m, _, _ := newTestManager(t)
			m.Deps.ProbeInterval = interval
			remotes := answerDocker(m, "1.47 27.3.1\n", nil)
			if err := m.waitDocker(context.Background(), &State{SSHPort: 42022}, time.Second); err == nil {
				t.Fatal("waitDocker with a non-positive interval returned nil")
			}
			if len(*remotes) != 0 {
				t.Fatalf("SSH commands = %v, want none", *remotes)
			}
		})
	}
}
