package vm

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"slices"
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
		if want := []string{"list", "--json", "omac"}; !slices.Equal(args, want) {
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

func TestDefaultLimaCtlWithoutBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := DefaultLimaCtl(context.Background(), t.TempDir(), nil, "list"); err == nil {
		t.Fatal("DefaultLimaCtl without limactl on PATH returned nil")
	}
}
