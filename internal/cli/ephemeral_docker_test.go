package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/ephemeraldocker"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

func TestParseLaunchArgsEphemeralDocker(t *testing.T) {
	opts, code := parseLaunchArgs("start", []string{"--ephemeral-docker"}, devnullEnv(t))
	if code != ExitOK {
		t.Fatalf("parseLaunchArgs() code = %d, want ExitOK", code)
	}
	if !opts.ephemeralDocker {
		t.Error("ephemeralDocker = false, want true")
	}
	// continue/resume share the parser, so the flag rides along there too.
	copts, code := parseLaunchArgs("continue", []string{"--ephemeral-docker"}, devnullEnv(t))
	if code != ExitOK || !copts.ephemeralDocker {
		t.Errorf("continue parse: code=%d ephemeralDocker=%v", code, copts.ephemeralDocker)
	}
}

func TestParseLaunchArgsRejectsEphemeralDockerWithoutSandbox(t *testing.T) {
	if _, code := parseLaunchArgs("start", []string{"--ephemeral-docker", "--no-sandbox"}, devnullEnv(t)); code == ExitOK {
		t.Error("parseLaunchArgs() succeeded with --ephemeral-docker and --no-sandbox")
	}
}

func TestEphemeralDockerExitCodes(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("boot: %w", ephemeraldocker.ErrLimaMissing), ExitPrerequisiteMissing},
		{fmt.Errorf("boot: %w", ephemeraldocker.ErrUnpinnedArch), ExitPrerequisiteMissing},
		{errors.New("ephemeral-docker: docker endpoint did not answer"), ExitGeneric},
		{fmt.Errorf("boot: %w", ephemeraldocker.ErrNoFreePort), ExitGeneric},
	}
	for _, c := range cases {
		if got := ephemeralDockerExitCode(c.err); got != c.want {
			t.Errorf("ephemeralDockerExitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

// fakeEphemeralSession is the CLI wiring test's stand-in for a booted VM.
type fakeEphemeralSession struct {
	mu       sync.Mutex
	teardown int
}

func (f *fakeEphemeralSession) Port() int { return 31234 }

func (f *fakeEphemeralSession) DockerHost() string { return "tcp://127.0.0.1:31234" }

func (f *fakeEphemeralSession) Teardown() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teardown++
	return nil
}

func (f *fakeEphemeralSession) teardowns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.teardown
}

// ephemeralDockerCaptureFixture mirrors the launch-args fixture: a capture
// profile whose command dumps its argv and DOCKER_HOST, so the launch
// pipeline runs end-to-end without a real sandbox backend and the script
// records what the wiring injected. Sandbox profiles are trusted only
// from the global config (workdir-local ones are stripped), so the
// profile is written to the isolated HOME's config.
func ephemeralDockerCaptureFixture(t *testing.T, script string) (workdir, capturePath string) {
	t.Helper()
	isolateHome(t)
	shortTmp, err := os.MkdirTemp("/tmp", "omac-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shortTmp) })
	t.Setenv("TMPDIR", shortTmp)

	workdir = t.TempDir()
	capturePath = filepath.Join(t.TempDir(), "capture")
	if err := os.WriteFile(capturePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(os.Getenv("HOME"), ".config", "omac", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	configText := fmt.Sprintf("sandbox:\n  default_profile: capture\n  profiles:\n    capture:\n      command: [%q, %q, %q, %q]\n",
		capturePath, "--", "{{inner_cmd}}", "{{inner_args}}")
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	return workdir, capturePath
}

func TestRunLaunchEphemeralDockerWiring(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	dump := filepath.Join(t.TempDir(), "dump")
	script := "#!/bin/sh\nprintf 'ARGV:%s\\n' \"$*\" >" + dump + "\nprintf 'DOCKER_HOST=%s\\n' \"$DOCKER_HOST\" >>" + dump + "\n"
	workdir, capturePath := ephemeralDockerCaptureFixture(t, script)
	env, stderr := launchTestEnv(t, workdir)
	harness, ok := config.LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness missing")
	}

	fake := &fakeEphemeralSession{}
	orig := newEphemeralDockerSession
	newEphemeralDockerSession = func(context.Context, ephemeraldocker.SessionOpts) (ephemeraldocker.Handle, error) {
		return fake, nil
	}
	t.Cleanup(func() { newEphemeralDockerSession = orig })

	code := runLaunch(env, launchOpts{
		label:            "start",
		harness:          harness,
		ephemeralDocker:  true,
		innerCmdOverride: capturePath,
	})
	if code != ExitOK {
		t.Fatalf("runLaunch code = %d, want ExitOK\nstderr:\n%s", code, stderr())
	}

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("capture dump missing: %v", err)
	}
	dumpStr := string(raw)
	if !strings.Contains(dumpStr, "DOCKER_HOST=tcp://127.0.0.1:31234") {
		t.Errorf("DOCKER_HOST not exported to the sandboxed process:\n%s", dumpStr)
	}
	if !strings.Contains(dumpStr, "--allow-env DOCKER_HOST") {
		t.Errorf("DOCKER_HOST not allowed through the profile's env filter:\n%s", dumpStr)
	}
	if !strings.Contains(dumpStr, "--open-port 31234") {
		t.Errorf("the docker forward port is not opened for the sandboxed process:\n%s", dumpStr)
	}
	if fake.teardowns() != 1 {
		t.Errorf("teardown calls = %d, want exactly 1 after the session ends", fake.teardowns())
	}
}

func TestRunLaunchEphemeralDockerBootFailure(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	workdir, capturePath := ephemeralDockerCaptureFixture(t, "#!/bin/sh\ntrue\n")
	env, _ := launchTestEnv(t, workdir)
	harness, ok := config.LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness missing")
	}

	orig := newEphemeralDockerSession
	newEphemeralDockerSession = func(context.Context, ephemeraldocker.SessionOpts) (ephemeraldocker.Handle, error) {
		return nil, fmt.Errorf("omac start: %w", ephemeraldocker.ErrLimaMissing)
	}
	t.Cleanup(func() { newEphemeralDockerSession = orig })

	code := runLaunch(env, launchOpts{
		label:            "start",
		harness:          harness,
		ephemeralDocker:  true,
		innerCmdOverride: capturePath,
	})
	if code != ExitPrerequisiteMissing {
		t.Fatalf("runLaunch code = %d, want ExitPrerequisiteMissing for a missing limactl", code)
	}
}

func TestRunLaunchEphemeralDockerNeedsSandbox(t *testing.T) {
	// Programmatic launchOpts can bypass the parser; the pipeline itself
	// must still refuse --no-sandbox + ephemeral-docker.
	isolateHome(t)
	env := makeEnv(t.TempDir())
	code := runLaunch(env, launchOpts{
		label:           "start",
		harness:         config.DefaultHarness(),
		ephemeralDocker: true,
		noSandbox:       true,
	})
	if code != ExitMisuse {
		t.Fatalf("code = %d, want ExitMisuse", code)
	}
}
