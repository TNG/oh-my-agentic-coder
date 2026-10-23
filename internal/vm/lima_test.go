package vm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFindLimactlMissing(t *testing.T) {
	lookPath := func(string) (string, error) { return "", exec.ErrNotFound }
	_, err := FindLimactl(lookPath)
	if !errors.Is(err, ErrLimaMissing) {
		t.Fatalf("err = %v, want ErrLimaMissing", err)
	}
}

func TestFindLimactlFound(t *testing.T) {
	var asked string
	lookPath := func(name string) (string, error) { asked = name; return "/opt/bin/limactl", nil }
	got, err := FindLimactl(lookPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/opt/bin/limactl" || asked != "limactl" {
		t.Errorf("got %q (asked %q)", got, asked)
	}
}

// fakeLima returns a LimaCtl that checks limaHome and the args, then writes out.
func fakeLima(t *testing.T, wantHome, out string, retErr error) LimaCtl {
	t.Helper()
	return func(_ context.Context, limaHome string, stdout io.Writer, args ...string) error {
		if limaHome != wantHome {
			t.Errorf("limaHome = %q, want %q", limaHome, wantHome)
		}
		if want := []string{"list", "--json"}; !slices.Equal(args, want) {
			t.Errorf("args = %v, want %v", args, want)
		}
		if stdout != nil {
			_, _ = io.WriteString(stdout, out)
		}
		return retErr
	}
}

func TestInspectParsesInstance(t *testing.T) {
	run := fakeLima(t, "/lh", `{"name":"omac","status":"Stopped","sshLocalPort":42022}`+"\n", nil)
	inst, err := Inspect(context.Background(), run, "/lh")
	if err != nil {
		t.Fatal(err)
	}
	if inst == nil {
		t.Fatal("Inspect returned nil instance")
	}
	want := Instance{Name: "omac", Status: StatusStopped, SSHLocalPort: 42022}
	if *inst != want {
		t.Errorf("instance = %+v, want %+v", *inst, want)
	}
}

func TestInspectSkipsForeignInstances(t *testing.T) {
	out := `{"name":"other","status":"Running","sshLocalPort":60022}` + "\n" +
		`{"name":"omac","status":"Running","sshLocalPort":42022}` + "\n"
	inst, err := Inspect(context.Background(), fakeLima(t, "/lh", out, nil), "/lh")
	if err != nil {
		t.Fatal(err)
	}
	want := Instance{Name: "omac", Status: StatusRunning, SSHLocalPort: 42022}
	if inst == nil || *inst != want {
		t.Fatalf("instance = %+v, want %+v", inst, want)
	}
}

func TestInspectOnlyForeignInstanceMeansAbsent(t *testing.T) {
	out := `{"name":"other","status":"Running"}` + "\n"
	inst, err := Inspect(context.Background(), fakeLima(t, "/lh", out, nil), "/lh")
	if err != nil || inst != nil {
		t.Fatalf("Inspect = %+v, %v; want nil, nil", inst, err)
	}
}

func TestInspectEmptyOutputMeansAbsent(t *testing.T) {
	run := fakeLima(t, "/lh", "", nil)
	inst, err := Inspect(context.Background(), run, "/lh")
	if err != nil || inst != nil {
		t.Fatalf("Inspect = %+v, %v; want nil, nil", inst, err)
	}
}

func TestInspectPropagatesError(t *testing.T) {
	boom := errors.New("boom")
	run := fakeLima(t, "/lh", "", boom)
	_, err := Inspect(context.Background(), run, "/lh")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped boom", err)
	}
}

func TestInspectRejectsGarbage(t *testing.T) {
	run := fakeLima(t, "/lh", "not json\n", nil)
	if _, err := Inspect(context.Background(), run, "/lh"); err == nil {
		t.Fatal("Inspect accepted non-JSON output")
	}
}

// Before the VM exists, `limactl list` prints "No instance found. Run
// limactl create" to stderr, which points users at the wrong tool. A call
// that captures stdout keeps stderr for its error; other calls show
// limactl's progress on omac's stderr.
func TestDefaultLimaCtlStderr(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho 'No instance found. Run limactl create' >&2\n[ \"$1\" = fail ] && exit 3\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "limactl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	errFile, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = errFile
	t.Cleanup(func() { os.Stderr = orig })
	printed := func() string {
		b, err := os.ReadFile(errFile.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	ctx := context.Background()

	var out bytes.Buffer
	if err := DefaultLimaCtl(ctx, t.TempDir(), &out, "list", "--json"); err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := printed(); got != "" {
		t.Errorf("successful list printed %q to stderr, want nothing", got)
	}
	err = DefaultLimaCtl(ctx, t.TempDir(), &out, "fail")
	if err == nil || !strings.Contains(err.Error(), "No instance found") {
		t.Errorf("failing captured call = %v, want limactl's stderr in the error", err)
	}
	if got := printed(); got != "" {
		t.Errorf("failing captured call printed %q, want it only in the error", got)
	}
	if err := DefaultLimaCtl(ctx, t.TempDir(), nil, "start"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := printed(); !strings.Contains(got, "No instance found") {
		t.Errorf("uncaptured call printed %q, want limactl's stderr passed through", got)
	}
}

func TestDefaultLimaCtlWithoutBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := DefaultLimaCtl(context.Background(), t.TempDir(), nil, "list"); err == nil {
		t.Fatal("DefaultLimaCtl without limactl on PATH returned nil")
	}
}
