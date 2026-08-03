package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
	"github.com/tngtech/oh-my-agentic-coder/internal/credproxy"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
)

// TestRunBuild_MissingRegistryCredentialDenial asserts criterion 7: a
// build with an APPROVED private registry alias but NO keychain credential
// for it fails closed with ExitBuildPolicyDenied (3) and a structured
// diagnostic naming the alias — never the credential, never a crash. The
// credential lookup is faked to return "missing", so no real keychain is
// touched. The manifest gate must pass first (approval pre-seeded), so
// the denial comes from the credential-lift startup, not the gate.
func TestRunBuild_MissingRegistryCredentialDenial(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	wt := t.TempDir()
	// Wrapper at root backend/ so Resolve passes.
	if err := os.MkdirAll(filepath.Join(wt, "backend"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "backend", "gradlew"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Manifest declaring an approved private registry (non-secret).
	if err := os.MkdirAll(filepath.Join(wt, ".omac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".omac", "build.yaml"), []byte(`version: 1
builds:
  - root: backend
registries:
  - alias: internal
    upstream: https://maven.internal.example/repo
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-seed the manifest approval so the gate passes unattended. The
	// approval record lives under the cache leaf's .omac-control/; the
	// build path resolves the same leaf via prepareBuildCache. We must
	// use the SAME cache dir the build path resolves — replicate the
	// resolution by reading the launcher config the same way build.go
	// does. Simpler: temporarily set the credential lookup to "missing"
	// and let the build path run; the gate is the first hurdle, and it
	// RECORDS approval on first run then fails. To avoid the two-run
	// dance, pre-seed the approval against the digest the build path will
	// compute. We resolve the cache leaf via the same helpers build.go
	// uses.
	cacheDir, closeScope, err := prepareBuildCache(wt, "")
	if err != nil {
		t.Fatalf("prepareBuildCache: %v", err)
	}
	closeScope()
	leaf := buildrun.GradleLeaf(cacheDir)
	manifest, err := buildmanifest.Load(wt)
	if err != nil {
		t.Fatal(err)
	}
	hostPolicy := buildrun.HostPolicy(0)
	if err := manifest.Validate(hostPolicy); err != nil {
		t.Fatal(err)
	}
	caps := manifest.CapabilitySet(hostPolicy)
	digest := buildmanifest.Digest(manifest)
	if err := buildmanifest.Approve(leaf, digest, caps); err != nil {
		t.Fatalf("pre-seed approval: %v", err)
	}
	// Also set the active record so the gate matches unattended. Approve
	// already stores active, so this is redundant but harmless.

	// Fake the credential lookup to return "missing" for every alias —
	// no real keychain touched. The credential-lift startup must produce
	// a *RegistryCredentialError naming the alias.
	origLookup := credentialLookup
	credentialLookup = func(alias string) (secrets.Secret, error) {
		return secrets.Secret{}, credproxy.ErrCredentialMissing
	}
	t.Cleanup(func() { credentialLookup = origLookup })

	env := &Env{Version: "test", Workdir: wt, Stdout: newDevNull(t)}
	cap := newCapture(t)
	env.Stderr = cap
	code := runBuild([]string{"--root", "backend", "--", "gradle", ":help"}, env)
	_ = cap.Sync()
	out, _ := os.ReadFile(cap.Name())

	if code != ExitBuildPolicyDenied {
		t.Errorf("code = %d, want %d (ExitBuildPolicyDenied)\nstderr:\n%s", code, ExitBuildPolicyDenied, out)
	}
	// The diagnostic must name the alias AND the keychain service
	// convention, WITHOUT the credential (which never existed here).
	if !strings.Contains(string(out), "internal") {
		t.Errorf("denial must name the registry alias 'internal':\n%s", out)
	}
	if !strings.Contains(string(out), "omac/build/registry/internal") {
		t.Errorf("denial must name the keychain service convention:\n%s", out)
	}
	// The spec-exact "current session policy is frozen; do not retry"
	// fragment must be present (spec.md:241/:313) — the end-to-end denial
	// must tell the agent retrying in the frozen session cannot succeed.
	if !strings.Contains(string(out), "do not retry") {
		t.Errorf("denial must state 'do not retry' (frozen-session policy):\n%s", out)
	}
	// Must not be a crash (service failure 10) — the denial is structured.
	if strings.Contains(string(out), "panic") {
		t.Errorf("denial must not crash:\n%s", out)
	}
	// The denial must come from the credential-lookup path (the
	// RegistryCredentialError), not from a NewServerWithConfig error —
	// proving the wiring reaches the lookup before NewServerWithConfig.
	if !strings.Contains(string(out), "denied private registry") {
		t.Errorf("denial must describe a credential-lookup failure (not a NewServerWithConfig error):\n%s", out)
	}
}

// TestStartCredentialProxy_WiresNewServerWithConfig asserts that
// startCredentialProxy with a valid credential lookup wires
// NewServerWithConfig correctly: it calls the 5-arg signature and returns
// a non-empty URL map with loopback URLs per alias (ticket 02).
func TestStartCredentialProxy_WiresNewServerWithConfig(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("credential proxy is macOS-only in v1")
	}

	wt := t.TempDir()
	// Stub wrapper so Resolve passes in the credential-lift path.
	if err := os.MkdirAll(filepath.Join(wt, "backend"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "backend", "gradlew"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	origLookup := credentialLookup
	credentialLookup = func(alias string) (secrets.Secret, error) {
		return secrets.NewSecretString("user:pass"), nil
	}
	t.Cleanup(func() { credentialLookup = origLookup })

	env := &Env{
		Version: "test",
		Workdir: wt,
		Stdout:  newDevNull(t),
		Stderr:  newDevNull(t),
	}
	worktree := t.TempDir()
	controlLeaf := t.TempDir()
	manifestRegistries := []buildmanifest.RegistryEntry{
		{Alias: "internal", Upstream: "https://maven.internal.example/repo"},
	}
	approvedAliases := []string{"internal"}

	urls, stop, err := startCredentialProxy(env, worktree, controlLeaf, manifestRegistries, approvedAliases)
	if err != nil {
		t.Fatalf("startCredentialProxy: %v", err)
	}
	if stop == nil {
		t.Fatal("stop func must be non-nil when credentials are approved")
	}
	defer stop()

	// URL map must be non-empty.
	if len(urls) == 0 {
		t.Fatal("URL map must be non-empty")
	}
	// At least one URL must be present for "internal".
	u, ok := urls["internal"]
	if !ok {
		t.Fatalf("URL map must contain alias 'internal'; got %v", urls)
	}
	// The URL must be a loopback URL with port and alias path.
	if !strings.HasPrefix(u, "http://127.0.0.1:") {
		t.Errorf("URL must be a loopback http URL: %q", u)
	}
	if !strings.HasSuffix(u, "/internal/") {
		t.Errorf("URL must end with /<alias>/: %q", u)
	}
	if strings.Contains(u, "@") {
		t.Errorf("URL must not contain userinfo: %q", u)
	}
}
