package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunBuildManifestDenials verifies the ticket-05 manifest policy-denial
// side of `omac build`: a committed manifest with a secret, a forbidden
// field, an absolute root, or a resource request above the host ceiling is
// rejected with ExitBuildPolicyDenied (3) and a structured stderr message,
// BEFORE any build code runs. These run unconditionally (no kernel sandbox
// needed) because the denial is at Load/Validate, before GrantsFor/RunBuild.
func TestRunBuildManifestDenials(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// makeWrapper creates an executable gradlew at <wt>/<root>/gradlew so
	// Resolve succeeds and the build reaches the manifest gate.
	makeWrapper := func(t *testing.T, wt, root string) {
		t.Helper()
		dir := filepath.Join(wt, root)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "gradlew"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest := func(t *testing.T, wt, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(wt, ".omac"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, ".omac", "build.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name     string
		manifest string
		root     string
		wantSub  string
	}{
		{
			name: "secret field rejected",
			manifest: `version: 1
builds:
  - root: backend
registries:
  - alias: internal
    upstream: ghcr.io/tng
    password: hunter2
`,
			root:    "backend",
			wantSub: "secret field rejected",
		},
		{
			name: "absolute root rejected",
			manifest: `version: 1
builds:
  - root: /Users/me/backend
`,
			root:    ".", // --root . (wrapper at worktree root); manifest's absolute root is rejected at Load
			wantSub: "absolute root",
		},
		{
			name: "forbidden bindMounts rejected",
			manifest: `version: 1
builds:
  - root: backend
    containers:
      images: [postgres:17]
      bindMounts: [/Users/me/.ssh]
`,
			root:    "backend",
			wantSub: "forbidden by host policy",
		},
		{
			name: "resource above ceiling rejected",
			manifest: `version: 1
resources:
  maxHeap: 8g
`,
			root:    "backend",
			wantSub: "exceeds host ceiling",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wt := t.TempDir()
			// Always create a wrapper at the --root path so Resolve passes
			// and the build reaches the manifest gate.
			makeWrapper(t, wt, c.root)
			// Also create a backend wrapper for cases that reference it.
			if c.root != "backend" {
				makeWrapper(t, wt, "backend")
			}
			if c.manifest != "" {
				writeManifest(t, wt, c.manifest)
			}
			env := &Env{
				Version: "test",
				Workdir: wt,
				Stdout:  newDevNull(t),
			}
			cap := newCapture(t)
			env.Stderr = cap
			code := runBuild([]string{"--root", c.root, "--", "gradle", ":help"}, env)
			_ = cap.Sync()
			out, _ := os.ReadFile(cap.Name())
			if code != ExitBuildPolicyDenied {
				t.Errorf("code = %d, want %d (ExitBuildPolicyDenied)\nstderr:\n%s", code, ExitBuildPolicyDenied, out)
			}
			if !strings.Contains(string(out), c.wantSub) {
				t.Errorf("stderr missing %q:\n%s", c.wantSub, out)
			}
		})
	}
}

// TestRunBuildNoManifestProceedsToBuild verifies criterion 1: a standard
// Gradle project with NO `.omac/build.yaml` proceeds past the manifest gate
// (the gate is skipped entirely when there is no manifest). The build then
// reaches GrantsFor / RunBuild; in-sandbox the kernel sandbox is unavailable
// so RunBuild fails as a SERVICE failure (10), NOT a policy denial (3). The
// assertion is that the manifest gate did NOT block the build (no manifest-
// related stderr), and the failure is downstream (sandbox/exec), proving the
// no-manifest path is the normal unattended case.
func TestRunBuildNoManifestProceedsToBuild(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "backend"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "backend", "gradlew"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := &Env{Version: "test", Workdir: wt, Stdout: newDevNull(t)}
	cap := newCapture(t)
	env.Stderr = cap
	code := runBuild([]string{"--root", "backend", "--", "gradle", ":help"}, env)
	_ = cap.Sync()
	out, _ := os.ReadFile(cap.Name())
	// The manifest gate must NOT have blocked the build: no manifest-related
	// stderr. The build proceeds to GrantsFor/RunBuild.
	if strings.Contains(string(out), "manifest approval required") {
		t.Errorf("no-manifest build must not hit the manifest gate:\n%s", out)
	}
	if strings.Contains(string(out), "no prior approval") {
		t.Errorf("no-manifest build must not require approval:\n%s", out)
	}
	// The build did NOT exit as a policy denial at the manifest stage. It
	// may exit 10 (service: sandbox unavailable in-sandbox) or 3 (if the
	// sandbox/exec path itself denies), but NOT with a manifest-gate
	// message. A manifest-gate denial is always exit 3 WITH a manifest
	// stderr message, so a no-manifest build returning 3 must NOT carry
	// one — assert that invariant so a regression where the no-manifest
	// path accidentally hits the gate is caught.
	if code == ExitBuildPolicyDenied &&
		(strings.Contains(string(out), "manifest approval required") ||
			strings.Contains(string(out), "no prior approval") ||
			strings.Contains(string(out), "manifest changed")) {
		t.Errorf("no-manifest build exited %d with a manifest-gate message (the gate must be skipped when there is no manifest):\n%s", code, out)
	}
}
