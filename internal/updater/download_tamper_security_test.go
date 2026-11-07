package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Install-time artifact integrity.
//
// Check verifies the downloaded artifact against the release's published
// checksums, but Apply installs from plan.LocalPath later — and bytes on
// disk are not bound to what an earlier read of them observed. Apply must
// therefore re-verify the digest of what it actually installs against the
// digest Check captured, and refuse outright when the plan carries no
// digest, so no Plan can reach an install unverified.

// --- self-contained fixtures (nothing borrowed from other test files) ------

type securityReleaseSource struct {
	rel Release
}

func (s securityReleaseSource) LatestRelease(context.Context) (Release, error) {
	return s.rel, nil
}

// securityFetcher serves fixed bytes per URL, never touching the network.
type securityFetcher struct {
	files map[string][]byte
}

func (f *securityFetcher) FetchAll(_ context.Context, url string) ([]byte, error) {
	body, ok := f.files[url]
	if !ok {
		return nil, fmt.Errorf("securityFetcher: no fixture for %s", url)
	}
	return body, nil
}

func (f *securityFetcher) FetchToFile(_ context.Context, url, dir, pattern string) (string, error) {
	body, ok := f.files[url]
	if !ok {
		return "", fmt.Errorf("securityFetcher: no fixture for %s", url)
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := tmp.Write(body); err != nil {
		return "", err
	}
	return tmp.Name(), nil
}

type securityCall struct {
	name string
	args []string
}

// securityRunner records every command Apply tries to run.
type securityRunner struct {
	calls []securityCall
}

func (r *securityRunner) Run(_ context.Context, name string, args []string, _ io.Reader, _, _ io.Writer) error {
	r.calls = append(r.calls, securityCall{name: name, args: args})
	return nil
}

// securityChecksums builds a sha256sum-style checksums.txt body covering
// exactly the given bytes, so Check's verification is legitimately satisfied.
func securityChecksums(name string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name))
}

// securityWriteTarGz writes a gzip-compressed tar at path holding a single
// regular file entry.
func securityWriteTarGz(t *testing.T, path, entry string, content []byte, mode int64) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: entry, Mode: mode, Size: int64(len(content))}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("write tar content: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write tar.gz file: %v", err)
	}
}

func securityDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		Executable: func() (string, error) { return filepath.Join(t.TempDir(), "omac"), nil },
		TempDir:    t.TempDir(),
		Stdin:      bytes.NewReader(nil),
		Stdout:     io.Discard,
		Stderr:     io.Discard,
	}
}

// securityCheckPlan drives the real Check pipeline against a fake release
// whose checksums.txt legitimately covers the asset, so the returned plan
// carries a checksum-verified download. The fake source and fetcher are
// installed into the deps copy used for Check.
func securityCheckPlan(t *testing.T, deps Deps, assetName string, body []byte) Plan {
	t.Helper()
	sumsURL := "https://example.invalid/checksums.txt"
	assetURL := "https://example.invalid/" + assetName
	deps.Source = securityReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
		{Name: assetName, BrowserDownloadURL: assetURL},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	deps.Fetcher = &securityFetcher{files: map[string][]byte{
		sumsURL:  securityChecksums(assetName, body),
		assetURL: body,
	}}
	plan, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !plan.ChecksumVerified {
		t.Fatalf("fixture: plan = %+v, want a checksum-verified download", plan)
	}
	return plan
}

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
		verifiedTgz := filepath.Join(dir, "verified.tar.gz")
		securityWriteTarGz(t, verifiedTgz, "omac", []byte("release-binary"), 0o755)
		verifiedBody, err := os.ReadFile(verifiedTgz)
		if err != nil {
			t.Fatal(err)
		}
		swappedTgz := filepath.Join(dir, "swapped.tar.gz")
		securityWriteTarGz(t, swappedTgz, "omac", []byte("other-binary"), 0o755)
		swappedBody, err := os.ReadFile(swappedTgz)
		if err != nil {
			t.Fatal(err)
		}

		deps := securityDeps(t)
		deps.GOOS, deps.GOARCH = "linux", "amd64"
		deps.SelfReplaceable = func(string) bool { return true }
		deps.Executable = func() (string, error) { return target, nil }
		deps.Replacer = fsSelfReplacer{}

		plan := securityCheckPlan(t, deps, "oh-my-agentic-coder_2.0.0_linux_x86_64.tar.gz", verifiedBody)
		if plan.Method != MethodTarballSelfReplace {
			t.Fatalf("fixture: method = %v, want MethodTarballSelfReplace", plan.Method)
		}

		// The download now holds different bytes than the ones Check verified.
		if err := os.WriteFile(plan.LocalPath, swappedBody, 0o600); err != nil {
			t.Fatal(err)
		}

		if err := Apply(context.Background(), plan, deps); !errors.Is(err, ErrChecksumMismatch) {
			t.Fatalf("Apply err = %v, want ErrChecksumMismatch", err)
		}
		installed, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if string(installed) != "current-binary" {
			t.Fatalf("running binary was rewritten despite the refused artifact: %q", installed)
		}
	})

	t.Run("linux package", func(t *testing.T) {
		runner := &securityRunner{}
		deps := securityDeps(t)
		deps.GOOS, deps.GOARCH = "linux", "amd64"
		deps.PkgManagers = []PackageManager{
			{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
		}
		deps.Runner = runner

		plan := securityCheckPlan(t, deps, "oh-my-agentic-coder_2.0.0_linux_x86_64.deb", []byte("verified-package"))
		if plan.Method != MethodLinuxPackage || plan.PackageManager != "dpkg" {
			t.Fatalf("fixture: plan = %+v, want a dpkg package install", plan)
		}

		// The download now holds different bytes than the ones Check verified.
		if err := os.WriteFile(plan.LocalPath, []byte("unverified-package"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := Apply(context.Background(), plan, deps); !errors.Is(err, ErrChecksumMismatch) {
			t.Fatalf("Apply err = %v, want ErrChecksumMismatch", err)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("package installer was invoked despite the refused artifact: %+v", runner.calls)
		}
	})
}

// TestSecurityApplyRejectsAbsentChecksum asserts that Apply refuses, via
// either install method, a Plan that carries no digest from Check — before
// any bytes are written or any installer is invoked.
func TestSecurityApplyRejectsAbsentChecksum(t *testing.T) {
	t.Run("self replace", func(t *testing.T) {
		dir := t.TempDir()
		tgzPath := filepath.Join(dir, "asset.tar.gz")
		securityWriteTarGz(t, tgzPath, "omac", []byte("new-binary"), 0o755)
		target := filepath.Join(dir, "omac")
		if err := os.WriteFile(target, []byte("current-binary"), 0o755); err != nil {
			t.Fatal(err)
		}

		deps := securityDeps(t)
		deps.Replacer = fsSelfReplacer{}
		deps.Executable = func() (string, error) { return target, nil }
		plan := Plan{
			Method:    MethodTarballSelfReplace,
			Asset:     Asset{Name: "asset.tar.gz"},
			LocalPath: tgzPath,
		}

		if err := Apply(context.Background(), plan, deps); err == nil {
			t.Fatal("Apply installed a plan carrying no verified digest; want refusal")
		}
		installed, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
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
		runner := &securityRunner{}
		deps := securityDeps(t)
		deps.PkgManagers = []PackageManager{
			{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
		}
		deps.Runner = runner
		plan := Plan{
			Method:         MethodLinuxPackage,
			PackageManager: "dpkg",
			Asset:          Asset{Name: "update.deb"},
			LocalPath:      pkgPath,
		}

		if err := Apply(context.Background(), plan, deps); err == nil {
			t.Fatal("Apply installed a plan carrying no verified digest; want refusal")
		}
		if len(runner.calls) != 0 {
			t.Fatalf("package installer was invoked despite the refusal: %+v", runner.calls)
		}
	})
}
