package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Install-time artifact integrity.
//
// Check verifies the downloaded artifact against the release's published
// checksums; Apply later installs from plan.LocalPath. What a path holds is
// not bound to what an earlier read of it observed, so Apply must re-verify
// the bytes it installs against the digest Check captured — and refuse
// outright when the plan carries no digest, so no Plan can reach an install
// unverified.

// TestSecurityApplyRejectsTamperedArtifact asserts that both install paths
// refuse bytes at plan.LocalPath that no longer match the checksum Check
// verified, and that the refusal happens before the running binary is
// rewritten or the package installer is invoked.
func TestSecurityApplyRejectsTamperedArtifact(t *testing.T) {
	t.Run("self replace", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "omac")
		if err := os.WriteFile(target, []byte("current-binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		releaseTgz := filepath.Join(dir, "release.tar.gz")
		writeTestTarGz(t, releaseTgz, "omac", []byte("release-binary"), 0o755)
		releaseBody, err := os.ReadFile(releaseTgz)
		if err != nil {
			t.Fatal(err)
		}
		otherTgz := filepath.Join(dir, "other.tar.gz")
		writeTestTarGz(t, otherTgz, "omac", []byte("other-binary"), 0o755)
		otherBody, err := os.ReadFile(otherTgz)
		if err != nil {
			t.Fatal(err)
		}

		assetName := "oh-my-agentic-coder_2.0.0_linux_x86_64.tar.gz"
		sumsURL := "https://example.invalid/checksums.txt"
		assetURL := "https://example.invalid/" + assetName
		deps := baseDeps(t)
		deps.Source = fakeReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
			{Name: assetName, BrowserDownloadURL: assetURL},
			{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
		}}}
		deps.Fetcher = &fakeFetcher{files: map[string][]byte{
			sumsURL:  checksumsFile(assetName, releaseBody),
			assetURL: releaseBody,
		}}
		deps.GOOS, deps.GOARCH = "linux", "amd64"
		deps.SelfReplaceable = func(string) bool { return true }
		deps.Executable = func() (string, error) { return target, nil }
		deps.Replacer = fsSelfReplacer{}

		plan, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if plan.Method != MethodTarballSelfReplace || !plan.ChecksumVerified {
			t.Fatalf("fixture: plan = %+v, want verified self-replace", plan)
		}

		// The download now holds different bytes than the ones Check verified.
		if err := os.WriteFile(plan.LocalPath, otherBody, 0o600); err != nil {
			t.Fatal(err)
		}

		err = Apply(context.Background(), plan, deps)
		if !errors.Is(err, ErrChecksumMismatch) {
			t.Fatalf("Apply err = %v, want ErrChecksumMismatch", err)
		}
		installed, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(installed) != "current-binary" {
			t.Fatalf("running binary was rewritten despite the rejected artifact: %q", installed)
		}
	})

	t.Run("linux package", func(t *testing.T) {
		releaseBody := []byte("verified-package")
		assetName := "oh-my-agentic-coder_2.0.0_linux_x86_64.deb"
		sumsURL := "https://example.invalid/checksums.txt"
		assetURL := "https://example.invalid/" + assetName
		runner := &fakeRunner{}
		deps := baseDeps(t)
		deps.Source = fakeReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
			{Name: assetName, BrowserDownloadURL: assetURL},
			{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
		}}}
		deps.Fetcher = &fakeFetcher{files: map[string][]byte{
			sumsURL:  checksumsFile(assetName, releaseBody),
			assetURL: releaseBody,
		}}
		deps.GOOS, deps.GOARCH = "linux", "amd64"
		deps.PkgManagers = []PackageManager{
			{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
		}
		deps.Runner = runner

		plan, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if plan.Method != MethodLinuxPackage || !plan.ChecksumVerified {
			t.Fatalf("fixture: plan = %+v, want verified package install", plan)
		}

		// The download now holds different bytes than the ones Check verified.
		if err := os.WriteFile(plan.LocalPath, []byte("unverified-package"), 0o600); err != nil {
			t.Fatal(err)
		}

		err = Apply(context.Background(), plan, deps)
		if !errors.Is(err, ErrChecksumMismatch) {
			t.Fatalf("Apply err = %v, want ErrChecksumMismatch", err)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("package installer was invoked despite the rejected artifact: %+v", runner.calls)
		}
	})
}

// TestSecurityApplyRejectsAbsentChecksum asserts that Apply refuses a Plan
// that carries no digest from Check, via either install method, before any
// bytes are written or any installer is invoked.
func TestSecurityApplyRejectsAbsentChecksum(t *testing.T) {
	t.Run("self replace", func(t *testing.T) {
		dir := t.TempDir()
		tgzPath := filepath.Join(dir, "asset.tar.gz")
		writeTestTarGz(t, tgzPath, "omac", []byte("new-binary"), 0o755)
		target := filepath.Join(dir, "omac")
		if err := os.WriteFile(target, []byte("current-binary"), 0o755); err != nil {
			t.Fatal(err)
		}

		deps := baseDeps(t)
		deps.Replacer = fsSelfReplacer{}
		deps.Executable = func() (string, error) { return target, nil }
		plan := Plan{
			Method:    MethodTarballSelfReplace,
			Asset:     Asset{Name: "asset.tar.gz"},
			LocalPath: tgzPath,
		}

		if err := Apply(context.Background(), plan, deps); err == nil {
			t.Fatal("Apply installed a plan with no verified digest; want refusal")
		}
		installed, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(installed) != "current-binary" {
			t.Fatalf("running binary was rewritten despite the refusal: %q", installed)
		}
	})

	t.Run("linux package", func(t *testing.T) {
		dir := t.TempDir()
		pkgPath := filepath.Join(dir, "update.deb")
		if err := os.WriteFile(pkgPath, []byte("package-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		runner := &fakeRunner{}
		deps := baseDeps(t)
		deps.Runner = runner
		deps.PkgManagers = []PackageManager{
			{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
		}
		plan := Plan{
			Method:         MethodLinuxPackage,
			PackageManager: "dpkg",
			Asset:          Asset{Name: "update.deb"},
			LocalPath:      pkgPath,
		}

		if err := Apply(context.Background(), plan, deps); err == nil {
			t.Fatal("Apply installed a plan with no verified digest; want refusal")
		}
		if len(runner.calls) != 0 {
			t.Fatalf("package installer was invoked despite the refusal: %+v", runner.calls)
		}
	})
}
