package sandboxrun

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxdeny"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// The marker written over a protected file also lands over the shell
// configs in the baseline protected set, so a login shell inside the
// sandbox sources it. Nothing used to source a masked file — the marker
// tests only read .netrc/.ssh, which are data — and the default text's
// "  POST $OMAC_BASE/sandbox/intent ..." line hung every bash invocation
// on hosts where /usr/bin/POST is libwww-perl's lwp-request, which
// blocks reading its request body from stdin (#213).
//
// These tests source the real production marker bytes with a stub POST
// on PATH, so the hang reproduces deterministically without libwww-perl
// installed. They need no sandbox: the issue reproduced outside one.

// postStubDir returns a directory holding an executable `POST` that
// blocks forever on stdin, standing in for lwp-request.
func postStubDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "POST")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ncat > /dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// sourceUnderStub sources path with bash, holding stdin open the way a
// harness does, and returns stdout, stderr and whether bash finished
// before the deadline.
func sourceUnderStub(t *testing.T, path string, timeout time.Duration) (stdout, stderr string, finished bool) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not installed: %v", err)
	}
	// An open pipe stands in for the harness's stdin: without it bash
	// inherits /dev/null and the stub returns immediately on EOF, hiding
	// the hang this test exists to catch.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", "source "+path+"; echo REACHED")
	cmd.Stdin = r
	cmd.Env = append(os.Environ(),
		"PATH="+postStubDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"OMAC_BASE=http://127.0.0.1:9",
	)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	// The stub POST outlives the bash the deadline kills and keeps the
	// inherited stdout pipe open, so Wait would block on the output-copy
	// goroutines forever. WaitDelay closes them shortly after the kill.
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	return out.String(), errb.String(), ctx.Err() == nil && err == nil
}

func TestMarkerFileIsInertWhenSourced(t *testing.T) {
	home := t.TempDir()
	profile := filepath.Join(home, ".profile")
	if err := os.WriteFile(profile, []byte("export SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &Grants{
		Workdir:        home,
		AllowPaths:     []string{home},
		ProtectedPaths: []string{profile},
		NetworkMode:    sandboxprofile.ModeBlocked,
		DenialText:     sandboxdeny.Default().MarkerFile,
	}
	cleanup, err := g.prepareMarkers()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	stdout, stderr, finished := sourceUnderStub(t, g.markerFile, 20*time.Second)
	if !finished {
		t.Fatalf("sourcing the marker did not complete: stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stdout, "REACHED") {
		t.Errorf("shell did not reach the end: stdout=%q stderr=%q", stdout, stderr)
	}
	if stderr != "" {
		t.Errorf("shell init noise from the marker: %q", stderr)
	}
}

// TestRawMarkerTextHangsWhenSourced proves the guard above can actually
// fail: the same text without neutralization is executable, so it hangs
// on the stub POST (or at minimum spews command-not-found).
func TestRawMarkerTextHangsWhenSourced(t *testing.T) {
	raw := filepath.Join(t.TempDir(), "raw")
	if err := os.WriteFile(raw, []byte(sandboxdeny.Default().MarkerFile), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, finished := sourceUnderStub(t, raw, 5*time.Second)
	if finished && stderr == "" {
		t.Errorf("raw marker text sourced cleanly — the inert test proves nothing: stdout=%q", stdout)
	}
}
