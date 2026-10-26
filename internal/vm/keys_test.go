package vm

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fakeKeygen records each call and creates the -f <path> and <path>.pub files
// the way ssh-keygen would.
type fakeKeygen struct {
	calls [][]string
}

func (f *fakeKeygen) run(_ context.Context, name string, args []string, _ io.Reader, _, _ io.Writer) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	i := slices.Index(args, "-f")
	if i < 0 || i+1 >= len(args) {
		return nil
	}
	path := args[i+1]
	if err := os.WriteFile(path, []byte("PRIVATE\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAAkey omac-vm\n"), 0o644)
}

func argAfter(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

func TestEnsureKeysGeneratesBothKeysOnce(t *testing.T) {
	l := NewLayout(filepath.Join(t.TempDir(), "vm"))
	fake := &fakeKeygen{}

	if err := EnsureKeys(context.Background(), l, fake.run); err != nil {
		t.Fatalf("EnsureKeys: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("calls = %d, want 2: %v", len(fake.calls), fake.calls)
	}
	for i, want := range []string{l.AdminKey, l.AgentKey} {
		call := fake.calls[i]
		if call[0] != "ssh-keygen" {
			t.Errorf("call %d binary = %q, want ssh-keygen", i, call[0])
		}
		args := call[1:]
		if v, _ := argAfter(args, "-t"); v != "ed25519" {
			t.Errorf("call %d: -t = %q, want ed25519", i, v)
		}
		if v, ok := argAfter(args, "-N"); !ok || v != "" {
			t.Errorf("call %d: -N = %q (present %v), want empty passphrase", i, v, ok)
		}
		if v, _ := argAfter(args, "-f"); v != want {
			t.Errorf("call %d: -f = %q, want %q", i, v, want)
		}
	}

	info, err := os.Stat(l.Root)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("root mode = %o, want 700", perm)
	}

	if err := EnsureKeys(context.Background(), l, fake.run); err != nil {
		t.Fatalf("second EnsureKeys: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Errorf("second run made calls: %v", fake.calls[2:])
	}
}

func TestEnsureKeysPropagatesRunnerError(t *testing.T) {
	l := NewLayout(filepath.Join(t.TempDir(), "vm"))
	failing := func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
		return os.ErrPermission
	}
	if err := EnsureKeys(context.Background(), l, failing); err == nil {
		t.Fatal("EnsureKeys with failing runner returned nil")
	}
}

func TestReadPubKeyTrims(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "admin_key")
	if err := os.WriteFile(key+".pub", []byte("  ssh-ed25519 AAAA omac-vm-admin \n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPubKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ssh-ed25519 AAAA omac-vm-admin"; got != want {
		t.Errorf("ReadPubKey = %q, want %q", got, want)
	}
}

func TestReadPubKeyRejectsMultiline(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	if err := os.WriteFile(key+".pub", []byte("ssh-ed25519 A\nssh-ed25519 B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPubKey(key); err == nil {
		t.Error("ReadPubKey accepted a two-line public key file")
	}
}

func TestReadPubKeyMissing(t *testing.T) {
	if _, err := ReadPubKey(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("ReadPubKey on missing file returned nil error")
	}
}
