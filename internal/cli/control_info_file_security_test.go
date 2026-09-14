//go:build vuln

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The serve control-info file.
//
// A running `omac serve` publishes where its control plane listens, so that
// later CLI invocations — `omac register`, `omac deregister`, `omac secrets` —
// can tell it to reload a directory. Those invocations run on the host, with
// the user's privileges and environment, outside any sandbox.
//
// The record sits at one fixed path in the system temp directory, which on
// Linux is the shared /tmp that the sandbox baseline grants the confined agent
// read-write. So the address a host process will POST to is read from a file
// the confined agent can rewrite, and nothing checks what it says before
// using it. Publishing it has the same problem from the other side: the record
// is staged at a fixed neighbouring path and written with an ordinary create,
// which follows a symlink already sitting there and writes through it.
//
// The file is a hint about a local service, not a capability. Treating it as
// one means a confined agent picks the destination of the host's requests, and
// picks a file for the host to overwrite.

// writeRawControlInfo plants a control-info file directly, as a process with
// write access to the shared temp directory can.
func writeRawControlInfo(t *testing.T, ci controlInfo) {
	t.Helper()
	data, err := json.Marshal(ci)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controlInfoPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSecurityControlInfoRejectsForeignControlBase asserts that a planted
// control base is not accepted as the address of the local serve process.
func TestSecurityControlInfoRejectsForeignControlBase(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	// Control: a genuine record — the shape serve itself writes — is read
	// back. Without it, a reader that rejected everything would look like a
	// fix while breaking `omac register` against a running serve.
	writeRawControlInfo(t, controlInfo{ControlBase: "http://127.0.0.1:45671", PID: os.Getpid()})
	ci, ok := readControlInfo()
	if !ok || ci.ControlBase != "http://127.0.0.1:45671" {
		t.Fatalf("a legitimate loopback control record was not read back (ok=%v, base=%q): the fixture is broken, not the security property", ok, ci.ControlBase)
	}

	for _, base := range []string{
		"http://attacker.example:8080",
		"http://169.254.169.254",
		"file:///etc/passwd",
	} {
		writeRawControlInfo(t, controlInfo{ControlBase: base, PID: os.Getpid()})
		ci, ok := readControlInfo()
		if !ok {
			continue // rejected: the property holds for this one
		}
		if ci.ControlBase == base {
			t.Errorf("a planted control base %q was accepted: host-side omac commands, running unsandboxed with the user's credentials, send their reload requests wherever the confined agent points them", base)
		}
	}
}

// TestSecurityControlInfoWriteDoesNotFollowSymlink asserts that publishing the
// record cannot be redirected into another file.
//
// The staging path is as predictable as the record's own, and creating a file
// there in the ordinary way follows a symlink that is already in place,
// truncating whatever it points at. Since the writer is the unsandboxed serve
// process, the target can be any file the user can write — a shell startup
// file being the obvious choice, as it turns the clobber into code the user
// runs at their next login.
func TestSecurityControlInfoWriteDoesNotFollowSymlink(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	victim := filepath.Join(t.TempDir(), "shell-startup-file")
	const original = "# the user's own file\n"
	if err := os.WriteFile(victim, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	// The staging path the publisher uses, squatted before serve starts.
	if err := os.Symlink(victim, controlInfoPath()+".tmp"); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if err := writeControlInfo("http://127.0.0.1:45671"); err != nil {
		t.Logf("writeControlInfo reported %v", err)
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim file: %v", err)
	}
	if string(got) != original {
		t.Errorf("publishing the control record overwrote an unrelated file through a planted symlink; it now contains %q: any file the user can write can be replaced by content of omac's making", string(got))
	}
}
