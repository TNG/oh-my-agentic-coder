package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

// These tests are the drift guard issue #156 asks for: each one asserts the
// value `omac diagnose --hash` prints against the REAL producer the runtime
// uses, never against a hardcoded digest. If someone re-inlines a sha256 in
// start.go/serve.go, or changes a cache identity, these fail.

// hashSandbox isolates HOME/XDG and points TMPDIR at a temp dir, then returns
// a fresh workdir. TMPDIR isolation matters: createRuntimeDir os.RemoveAll's
// its target, and these tests call it for real — without this it could delete
// a developer's live `omac start` runtime dir.
func hashSandbox(t *testing.T) string {
	t.Helper()
	isolateHome(t)
	t.Setenv("TMPDIR", t.TempDir())
	return t.TempDir()
}

// runHash invokes the real CLI entry point and decodes the JSON payload.
func runHash(t *testing.T, wd string, args ...string) (hashView, int, string) {
	t.Helper()
	env, read := captureEnv(t, wd)
	code := runDiagnose(append(args, "--json"), env)
	out, errOut := read()
	var view hashView
	if code == ExitOK {
		if err := json.Unmarshal([]byte(out), &view); err != nil {
			t.Fatalf("decode --hash JSON: %v; stdout=%q stderr=%q", err, out, errOut)
		}
	}
	return view, code, errOut
}

func onlyEntry(t *testing.T, view hashView, kind string) hashEntry {
	t.Helper()
	if len(view.Entries) != 1 {
		t.Fatalf("--hash=%s: got %d entries, want 1: %+v", kind, len(view.Entries), view.Entries)
	}
	if view.Entries[0].Kind != kind {
		t.Fatalf("--hash=%s: got kind %q", kind, view.Entries[0].Kind)
	}
	return view.Entries[0]
}

// TestDiagnoseHashRuntimeMatchesCreateRuntimeDir: the reported runtime path is
// byte-for-byte the directory createRuntimeDir actually creates.
func TestDiagnoseHashRuntimeMatchesCreateRuntimeDir(t *testing.T) {
	wd := hashSandbox(t)

	want, err := createRuntimeDir(wd)
	if err != nil {
		t.Fatalf("createRuntimeDir: %v", err)
	}

	view, code, errOut := runHash(t, wd, "--hash=runtime")
	if code != ExitOK {
		t.Fatalf("code = %d; stderr=%q", code, errOut)
	}
	got := onlyEntry(t, view, "runtime")
	if got.Path != want {
		t.Errorf("path = %q, want %q (createRuntimeDir drifted from runtimeDirPath)", got.Path, want)
	}
	if got.Digest != filepath.Base(want)[len("omac-"):] {
		t.Errorf("digest = %q, not the suffix of %q", got.Digest, want)
	}
	if got.Input != wd {
		t.Errorf("input = %q, want the absolute workdir %q", got.Input, wd)
	}
}

// TestDiagnoseHashServeMatchesCreateRuntimeDirServe: same guard for serve,
// whose identity carries the "serve:" prefix so it never collides with start's.
func TestDiagnoseHashServeMatchesCreateRuntimeDirServe(t *testing.T) {
	wd := hashSandbox(t)

	want, err := createRuntimeDirServe(wd)
	if err != nil {
		t.Fatalf("createRuntimeDirServe: %v", err)
	}

	view, code, errOut := runHash(t, wd, "--hash=serve")
	if code != ExitOK {
		t.Fatalf("code = %d; stderr=%q", code, errOut)
	}
	got := onlyEntry(t, view, "serve")
	if got.Path != want {
		t.Errorf("path = %q, want %q (createRuntimeDirServe drifted from serveRuntimeDirPath)", got.Path, want)
	}
	if got.Input != "serve:"+wd {
		t.Errorf("input = %q, want %q", got.Input, "serve:"+wd)
	}
	if got.Path == runtimeDirPath(wd) {
		t.Error("serve and start runtime dirs collide")
	}
}

// TestDiagnoseHashKeychainMatchesWorkdirID: the keychain kind is exactly what
// register/start/serve scope secrets under.
func TestDiagnoseHashKeychainMatchesWorkdirID(t *testing.T) {
	wd := hashSandbox(t)

	view, code, errOut := runHash(t, wd, "--hash=keychain")
	if code != ExitOK {
		t.Fatalf("code = %d; stderr=%q", code, errOut)
	}
	got := onlyEntry(t, view, "keychain")
	if want := keychain.WorkdirID(wd); got.Digest != want {
		t.Errorf("digest = %q, want keychain.WorkdirID = %q", got.Digest, want)
	}
	if got.Path != "" {
		t.Errorf("keychain id is a namespace, not a path; got path %q", got.Path)
	}
}

// TestDiagnoseHashCacheWorkdirScope: with cache.scope=workdir configured, the
// reported cache matches toolcache's own description of the workdir scope.
func TestDiagnoseHashCacheWorkdirScope(t *testing.T) {
	wd := hashSandbox(t)
	writeWorkdirScopeConfig(t, wd)

	want, err := toolcache.DescribePersistent(toolcache.DomainWorkdir, wd)
	if err != nil {
		t.Fatalf("DescribePersistent: %v", err)
	}

	view, code, errOut := runHash(t, wd, "--hash=cache")
	if code != ExitOK {
		t.Fatalf("code = %d; stderr=%q", code, errOut)
	}
	got := onlyEntry(t, view, "cache")
	if got.Digest != want.Digest {
		t.Errorf("digest = %q, want %q", got.Digest, want.Digest)
	}
	if got.Path != want.Dir {
		t.Errorf("path = %q, want %q", got.Path, want.Dir)
	}
	if got.Input != want.Identity {
		t.Errorf("input = %q, want the hashed identity %q", got.Input, want.Identity)
	}
}

// TestDiagnoseHashCacheDefaultsToShared: with no launcher config on disk the
// scope resolves to global/shared, i.e. the same cache for every workdir.
func TestDiagnoseHashCacheDefaultsToShared(t *testing.T) {
	wd := hashSandbox(t)

	want, err := toolcache.DescribeShared()
	if err != nil {
		t.Fatalf("DescribeShared: %v", err)
	}

	view, code, errOut := runHash(t, wd, "--hash=cache")
	if code != ExitOK {
		t.Fatalf("code = %d; stderr=%q", code, errOut)
	}
	got := onlyEntry(t, view, "cache")
	if got.Digest != want.Digest {
		t.Errorf("digest = %q, want the shared scope %q", got.Digest, want.Digest)
	}
	if got.Path != want.Dir {
		t.Errorf("path = %q, want %q", got.Path, want.Dir)
	}
}

// TestDiagnoseHashAgreesWithProvenance: `--hash=cache` and the already-shipped
// `omac provenance` cache view must name the same directory — they share
// describeCacheScope, and this pins that they keep sharing it.
func TestDiagnoseHashAgreesWithProvenance(t *testing.T) {
	wd := hashSandbox(t)
	writeWorkdirScopeConfig(t, wd)

	view, code, errOut := runHash(t, wd, "--hash=cache")
	if code != ExitOK {
		t.Fatalf("code = %d; stderr=%q", code, errOut)
	}

	pv, err := buildProvenanceView(wd, "")
	if err != nil {
		t.Fatalf("buildProvenanceView: %v", err)
	}
	if got := onlyEntry(t, view, "cache").Path; got != pv.Cache.Path {
		t.Errorf("--hash=cache path %q != provenance cache.path %q", got, pv.Cache.Path)
	}
}

// TestDiagnoseHashAllKinds: bare --hash emits every kind, in a stable order,
// and reports the TMPDIR the paths were resolved against.
func TestDiagnoseHashAllKinds(t *testing.T) {
	wd := hashSandbox(t)

	view, code, errOut := runHash(t, wd, "--hash")
	if code != ExitOK {
		t.Fatalf("code = %d; stderr=%q", code, errOut)
	}
	var kinds []string
	for _, e := range view.Entries {
		if e.Error != "" {
			t.Errorf("kind %s errored: %s", e.Kind, e.Error)
		}
		kinds = append(kinds, e.Kind)
	}
	if want := strings.Join(hashKinds, ","); strings.Join(kinds, ",") != want {
		t.Errorf("kinds = %v, want %v", kinds, hashKinds)
	}
	if view.Workdir != wd {
		t.Errorf("workdir = %q, want %q", view.Workdir, wd)
	}
	if view.TmpDir == "" {
		t.Error("tmpdir is empty; the runtime/serve paths are unattributable without it")
	}
}

// TestDiagnoseHashTextMode: the default (non-JSON) rendering names every kind
// and its digest.
func TestDiagnoseHashTextMode(t *testing.T) {
	wd := hashSandbox(t)

	env, read := captureEnv(t, wd)
	if code := runDiagnose([]string{"--hash"}, env); code != ExitOK {
		_, errOut := read()
		t.Fatalf("code = %d; stderr=%q", code, errOut)
	}
	out, _ := read()
	for _, kind := range hashKinds {
		if !strings.Contains(out, kind) {
			t.Errorf("text output missing kind %q; got:\n%s", kind, out)
		}
	}
	if !strings.Contains(out, runtimeDirPath(wd)) {
		t.Errorf("text output missing the runtime path; got:\n%s", out)
	}
	if !strings.Contains(out, keychain.WorkdirID(wd)) {
		t.Errorf("text output missing the keychain id; got:\n%s", out)
	}
}

// TestDiagnoseHashUnknownKind: a typo is rejected loudly, with the valid
// kinds listed, rather than silently printing everything.
func TestDiagnoseHashUnknownKind(t *testing.T) {
	wd := hashSandbox(t)

	env, read := captureEnv(t, wd)
	if code := runDiagnose([]string{"--hash=bogus"}, env); code != ExitMisuse {
		t.Fatalf("code = %d, want ExitMisuse", code)
	}
	_, errOut := read()
	if !strings.Contains(errOut, "bogus") {
		t.Errorf("stderr does not name the bad kind: %q", errOut)
	}
	for _, kind := range hashKinds {
		if !strings.Contains(errOut, kind) {
			t.Errorf("stderr does not list valid kind %q: %q", kind, errOut)
		}
	}
}

// TestDiagnoseHashRejectsSpaceForm: --hash is a bool-style flag so bare
// `--hash` works, which means `--hash runtime` parses as bare --hash plus a
// positional. That must fail loudly — silently printing all four kinds when
// the user asked for one is the trap this guards.
func TestDiagnoseHashRejectsSpaceForm(t *testing.T) {
	wd := hashSandbox(t)

	env, read := captureEnv(t, wd)
	if code := runDiagnose([]string{"--hash", "runtime"}, env); code != ExitMisuse {
		t.Fatalf("code = %d, want ExitMisuse", code)
	}
	_, errOut := read()
	if !strings.Contains(errOut, "--hash=runtime") {
		t.Errorf("stderr does not suggest the attached form: %q", errOut)
	}
}

// TestDiagnoseHashNeedsNoProfileOrAuditTrail: --hash must answer in a workdir
// that has never been started — no audit log, no sandbox profile. This pins
// the short-circuit ahead of sandboxprofile.Resolve in runDiagnose.
func TestDiagnoseHashNeedsNoProfileOrAuditTrail(t *testing.T) {
	wd := hashSandbox(t)

	env, read := captureEnv(t, wd)
	if code := runDiagnose([]string{"--hash=runtime", "--profile", "/nonexistent/profile.json"}, env); code != ExitOK {
		_, errOut := read()
		t.Fatalf("code = %d, want ExitOK; stderr=%q", code, errOut)
	}
}

// TestDiagnoseHashCreatesNothing: --hash is a read-only debugging aid. It must
// never create (or, worse, RemoveAll) the directories it names — reporting the
// runtime dir of a LIVE session must not wipe that session's logs and pids.
func TestDiagnoseHashCreatesNothing(t *testing.T) {
	wd := hashSandbox(t)

	// Stage a live-looking runtime dir with a log in it.
	live, err := createRuntimeDir(wd)
	if err != nil {
		t.Fatalf("createRuntimeDir: %v", err)
	}
	marker := filepath.Join(live, "logs", "skill.log")
	if err := os.WriteFile(marker, []byte("live session output"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	view, code, errOut := runHash(t, wd, "--hash")
	if code != ExitOK {
		t.Fatalf("code = %d; stderr=%q", code, errOut)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "live session output" {
		t.Fatalf("--hash destroyed the live runtime dir; read = %q, err = %v", got, err)
	}
	// The cache scope dir is likewise only described, never created.
	for _, e := range view.Entries {
		if e.Kind != "cache" {
			continue
		}
		if _, err := os.Stat(e.Path); err == nil {
			t.Errorf("--hash created the cache scope dir %q", e.Path)
		}
	}
}
