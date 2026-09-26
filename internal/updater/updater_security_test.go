package updater

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestSecurityUpdaterVerifiesSignature asserts that a release artifact
// whose checksum matches checksums.txt is still rejected when no trusted
// signature is present. A checksum fetched from the same release as the
// artifact is not authenticity: whoever can publish the artifact can also
// publish the matching checksum, so the download path must verify an
// independent signature before accepting the artifact.
func TestSecurityUpdaterVerifiesSignature(t *testing.T) {
	assetBody := []byte("package-bytes")
	assetName := "oh-my-agentic-coder_2.0.0_linux_x86_64.deb"
	sumsURL := "https://example.invalid/checksums.txt"
	assetURL := "https://example.invalid/" + assetName

	deps := baseDeps(t)
	deps.Source = fakeReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
		{Name: assetName, BrowserDownloadURL: assetURL},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	deps.Fetcher = &fakeFetcher{files: map[string][]byte{
		sumsURL:  checksumsFile(assetName, assetBody),
		assetURL: assetBody,
	}}
	deps.GOOS, deps.GOARCH = "linux", "amd64"
	deps.PkgManagers = []PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}

	_, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
	if err == nil {
		t.Fatalf("Check accepted an artifact with a valid checksum but no trusted signature")
	}
	// The checksum itself is valid, so the rejection must come from the
	// missing signature, not from a checksum mismatch.
	if errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Check rejected with ErrChecksumMismatch, but the checksum is valid; the rejection should come from signature verification")
	}
}

// TestSecurityUpdaterNoAllowUntrusted asserts that the apk install command
// never passes --allow-untrusted, which disables apk's own signature
// verification of the local package file.
func TestSecurityUpdaterNoAllowUntrusted(t *testing.T) {
	pms := DetectPackageManagers(func(name string) (string, error) {
		if name == "apk" {
			return "/sbin/apk", nil
		}
		return "", errors.New("not found")
	})
	var apk PackageManager
	for _, pm := range pms {
		if pm.Name == "apk" {
			apk = pm
			break
		}
	}
	if apk.Name == "" {
		t.Fatalf("control: DetectPackageManagers did not return apk despite it being on PATH")
	}
	for _, arg := range apk.InstallArgs("/tmp/x.apk") {
		if arg == "--allow-untrusted" {
			t.Errorf("apk InstallArgs includes --allow-untrusted, which disables apk signature verification of the local package")
		}
	}

	// Verify the same property through the Apply path.
	deps := baseDeps(t)
	runner := &fakeRunner{}
	deps.Runner = runner
	deps.PkgManagers = []PackageManager{apk}
	plan := Plan{Method: MethodLinuxPackage, PackageManager: "apk", LocalPath: "/tmp/x.apk"}
	if err := Apply(context.Background(), plan, deps); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, call := range runner.calls {
		for _, arg := range call.args {
			if arg == "--allow-untrusted" {
				t.Errorf("Apply invoked apk with --allow-untrusted: %v", call.args)
			}
		}
	}
}

// TestSecurityUpdaterReVerifiesBeforeInstall asserts that Apply rejects an
// artifact whose content changed after Check verified it. Without
// re-verification, anything written to the staging path between the two
// calls is installed with elevated privileges.
func TestSecurityUpdaterReVerifiesBeforeInstall(t *testing.T) {
	assetBody := []byte("legitimate-package")
	assetName := "oh-my-agentic-coder_2.0.0_linux_x86_64.deb"
	sumsURL := "https://example.invalid/checksums.txt"
	assetURL := "https://example.invalid/" + assetName

	makeDeps := func() (Deps, *fakeRunner) {
		deps := baseDeps(t)
		deps.Source = fakeReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
			{Name: assetName, BrowserDownloadURL: assetURL},
			{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
		}}}
		deps.Fetcher = &fakeFetcher{files: map[string][]byte{
			sumsURL:  checksumsFile(assetName, assetBody),
			assetURL: assetBody,
		}}
		deps.GOOS, deps.GOARCH = "linux", "amd64"
		deps.PkgManagers = []PackageManager{
			{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
		}
		runner := &fakeRunner{}
		deps.Runner = runner
		return deps, runner
	}

	ctx := context.Background()

	// Control: without tampering, Apply installs the verified artifact.
	deps, runner := makeDeps()
	plan, err := Check(ctx, Options{CurrentVersion: "1.0.0"}, deps)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if err := Apply(ctx, plan, deps); err != nil {
		t.Fatalf("control Apply: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("control: expected 1 installer call, got %d", len(runner.calls))
	}

	// The verified artifact is replaced before Apply runs.
	deps2, runner2 := makeDeps()
	plan2, err := Check(ctx, Options{CurrentVersion: "1.0.0"}, deps2)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if err := os.WriteFile(plan2.LocalPath, []byte("replaced-content"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err = Apply(ctx, plan2, deps2)
	if err == nil {
		t.Fatalf("Apply installed an artifact whose content changed after verification")
	}
	if len(runner2.calls) != 0 {
		t.Errorf("Apply invoked the installer despite the artifact being modified after verification")
	}
}
