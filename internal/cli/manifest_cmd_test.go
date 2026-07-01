package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRunManifestStdin(t *testing.T) {
	activateJSON := `{"dir":"/proj","state":"active","skills":[{"name":"echo","scope":"workdir","state":"ready","base":"http://127.0.0.1:9000/echo"}]}`
	env, outBuf, errBuf, drain := newPipeEnv(t, activateJSON)

	code := runManifest([]string{"--skills-dir", ".claude/skills"}, env)
	drain()
	if code != ExitOK {
		t.Fatalf("code=%d, want %d; stderr=%s", code, ExitOK, errBuf.String())
	}
	if !bytes.Contains(outBuf.Bytes(), []byte("echo")) {
		t.Errorf("output missing skill name; got:\n%s", outBuf.String())
	}
}

func TestRunManifestInputFile(t *testing.T) {
	activateJSON := `{"dir":"/proj","state":"active","skills":[]}`
	tmp := t.TempDir()
	f := filepath.Join(tmp, "activate.json")
	if err := os.WriteFile(f, []byte(activateJSON), 0644); err != nil {
		t.Fatal(err)
	}
	env, outBuf, errBuf, drain := newPipeEnv(t, "")

	code := runManifest([]string{"--skills-dir", ".codex/skills", "--input", f}, env)
	drain()
	if code != ExitOK {
		t.Fatalf("code=%d, want %d; stderr=%s", code, ExitOK, errBuf.String())
	}
	if !bytes.Contains(outBuf.Bytes(), []byte("/proj")) {
		t.Errorf("output missing dir; got:\n%s", outBuf.String())
	}
}

func TestRunManifestMissingSkillsDir(t *testing.T) {
	env, _, _, _ := newPipeEnv(t, "")
	code := runManifest([]string{}, env)
	if code != ExitMisuse {
		t.Fatalf("code=%d, want ExitMisuse(%d)", code, ExitMisuse)
	}
}

func TestRunManifestInputFileNotFound(t *testing.T) {
	env, _, _, _ := newPipeEnv(t, "")
	code := runManifest([]string{"--skills-dir", ".claude/skills", "--input", "/nonexistent-xyz.json"}, env)
	if code == ExitOK {
		t.Fatal("expected non-zero exit for missing input file")
	}
}

func TestRunManifestInvalidJSON(t *testing.T) {
	env, outBuf, _, drain := newPipeEnv(t, "{not valid json")

	code := runManifest([]string{"--skills-dir", ".claude/skills"}, env)
	drain()
	if code != ExitOK {
		t.Fatalf("code=%d, want ExitOK (Render returns \"\" on parse error)", code)
	}
	if outBuf.Len() != 0 {
		t.Errorf("expected empty output for invalid JSON; got:\n%s", outBuf.String())
	}
}

// pipeEnv wraps an Env with its pipe read-ends and output buffers.
type pipeEnv struct {
	env    *Env
	outR   *os.File
	errR   *os.File
	outBuf *bytes.Buffer
	errBuf *bytes.Buffer
}

// newPipeEnv builds an Env with stdin backed by an os.Pipe (data
// written and closed), and stdout/stderr backed by os.Pipe write-ends.
// Returns the Env, output buffer, error buffer, and a drain function.
// The drain function closes write-ends and synchronously copies output
// into the buffers. It must be called before reading the buffers.
func newPipeEnv(t *testing.T, stdinData string) (env *Env, outBuf, errBuf *bytes.Buffer, drain func()) {
	t.Helper()
	pe := &pipeEnv{
		outBuf: &bytes.Buffer{},
		errBuf: &bytes.Buffer{},
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if stdinData != "" {
		stdinW.WriteString(stdinData)
	}
	stdinW.Close()
	t.Cleanup(func() { stdinR.Close() })

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	pe.env = &Env{Stdin: stdinR, Stdout: outW, Stderr: errW, Version: "test", Workdir: "."}
	pe.outR = outR
	pe.errR = errR
	t.Cleanup(func() { outR.Close(); errR.Close() })

	drain = func() {
		outW.Close()
		errW.Close()
		io.Copy(pe.outBuf, pe.outR)
		io.Copy(pe.errBuf, pe.errR)
	}
	return pe.env, pe.outBuf, pe.errBuf, drain
}
