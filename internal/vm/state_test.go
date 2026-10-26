package vm

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSaveLoadStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	want := &State{
		Version: StateVersion, SSHPort: 60022, DockerPort: 62375, Mode: ModeOffline,
		Images: []string{"postgres:16"},
		JDKs:   []string{"temurin-25"}, Packages: []string{"jq"},
	}
	if err := SaveState(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadState = %+v, want %+v", got, want)
	}
}

func TestLoadStateMissingIsNil(t *testing.T) {
	got, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil || got != nil {
		t.Errorf("LoadState(missing) = %+v, %v; want nil, nil", got, err)
	}
}

func TestLoadStateIgnoresOldWorktreesField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, []byte(`{"mode":"offline","worktrees":["/w/a"],"jdks":["temurin-25"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(p)
	if err != nil || st.Mode != ModeOffline || len(st.JDKs) != 1 {
		t.Fatalf("LoadState = %+v, %v; want the other fields read", st, err)
	}
}

func TestLoadStateDefaultsVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, []byte(`{"mode":"online"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != StateVersion || got.Mode != ModeOnline {
		t.Errorf("LoadState = %+v, want version %d mode %q", got, StateVersion, ModeOnline)
	}
}

func TestSaveStateCreatesPrivateDirAndFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vm")
	p := filepath.Join(dir, "state.json")
	if err := SaveState(p, &State{Mode: ModeOffline}); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %o, want 600", perm)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %d entries", len(entries))
	}
}

func TestLoadStateRejectsCorruptJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(p); err == nil {
		t.Error("corrupt state.json accepted")
	}
}
