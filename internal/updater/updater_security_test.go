package updater

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

// Self-contained fakes for the security regression tests.  These duplicate
// the shapes of the helpers in updater_test.go but live entirely in this
// *_security_test.go file, so the security suite does not depend on
// non-security test helpers being present.

type secReleaseSource struct {
	rel Release
	err error
}

func (s secReleaseSource) LatestRelease(_ context.Context) (Release, error) {
	return s.rel, s.err
}

type secFetcher struct {
	files map[string][]byte
	paths []string
}

func (f *secFetcher) FetchAll(_ context.Context, url string) ([]byte, error) {
	body, ok := f.files[url]
	if !ok {
		return nil, fmt.Errorf("secFetcher: no fixture for %s", url)
	}
	return body, nil
}

func (f *secFetcher) FetchToFile(_ context.Context, url, dir, pattern string) (string, error) {
	body, ok := f.files[url]
	if !ok {
		return "", fmt.Errorf("secFetcher: no fixture for %s", url)
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := tmp.Write(body); err != nil {
		return "", err
	}
	f.paths = append(f.paths, tmp.Name())
	return tmp.Name(), nil
}

type secRunCall struct {
	name string
	args []string
}

type secRunner struct {
	calls []secRunCall
	err   error
}

func (r *secRunner) Run(_ context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	r.calls = append(r.calls, secRunCall{name: name, args: args})
	return r.err
}

func secChecksumsFile(name string, content []byte) []byte {
	sum := sha256.Sum256(content)
	return []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name))
}

// secSignatureVerifier is the security suite's test double for the production
// ed25519 verifier. It accepts any non-empty signature so the suite can drive
// the download/verify and re-verify-before-install paths without real
// cryptography; production wires verifyReleaseSignature via RealDeps.
func secSignatureVerifier(signed, sig []byte) error {
	if len(sig) == 0 {
		return errors.New("empty signature")
	}
	return nil
}

func secBaseDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		Executable:        func() (string, error) { return filepath.Join(t.TempDir(), "omac"), nil },
		TempDir:           t.TempDir(),
		SignatureVerifier: secSignatureVerifier,
		Stdin:             bytes.NewReader(nil),
		Stdout:            io.Discard,
		Stderr:            io.Discard,
	}
}

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

	deps := secBaseDeps(t)
	deps.Source = secReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
		{Name: assetName, BrowserDownloadURL: assetURL},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	deps.Fetcher = &secFetcher{files: map[string][]byte{
		sumsURL:  secChecksumsFile(assetName, assetBody),
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
	deps := secBaseDeps(t)
	runner := &secRunner{}
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
//
// Unlike TestSecurityUpdaterVerifiesSignature, this test's release includes
// a signature asset (checksums.txt.sig) so that Check can accept the
// artifact once signature verification is implemented. The two tests must
// not share identical fixtures: VerifiesSignature demands rejection of a
// signature-less release, while this test demands acceptance (so that
// Apply's pre-install re-verification is the thing under test).
func TestSecurityUpdaterReVerifiesBeforeInstall(t *testing.T) {
	assetBody := []byte("legitimate-package")
	assetName := "oh-my-agentic-coder_2.0.0_linux_x86_64.deb"
	sumsURL := "https://example.invalid/checksums.txt"
	sigURL := "https://example.invalid/checksums.txt.sig"
	assetURL := "https://example.invalid/" + assetName

	makeDeps := func() (Deps, *secRunner) {
		deps := secBaseDeps(t)
		deps.Source = secReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
			{Name: assetName, BrowserDownloadURL: assetURL},
			{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
			{Name: "checksums.txt.sig", BrowserDownloadURL: sigURL},
		}}}
		deps.Fetcher = &secFetcher{files: map[string][]byte{
			sumsURL:  secChecksumsFile(assetName, assetBody),
			sigURL:   []byte("dummy-signature"),
			assetURL: assetBody,
		}}
		deps.GOOS, deps.GOARCH = "linux", "amd64"
		deps.PkgManagers = []PackageManager{
			{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
		}
		runner := &secRunner{}
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

// TestSecurityReleaseSpecSignsChecksums asserts that .goreleaser.yaml
// produces a checksums.txt.sig signature file. Without a signing step in
// the release spec, the updater's signature verification always fails
// because no signature asset is published, making the entire signature
// check dead code rather than a security control.
func TestSecurityReleaseSpecSignsChecksums(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root not found (no go.mod): %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}
	var cfg struct {
		Signs []struct {
			Cmd       string   `yaml:"cmd"`
			Args      []string `yaml:"args"`
			Signature string   `yaml:"signature"`
			Artifacts string   `yaml:"artifacts"`
		} `yaml:"signs"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse .goreleaser.yaml: %v", err)
	}
	if len(cfg.Signs) == 0 {
		t.Fatal(".goreleaser.yaml has no signs: section; release would ship no checksums.txt.sig")
	}
	sign := cfg.Signs[0]
	if sign.Artifacts != "checksums" {
		t.Errorf("signs[0].artifacts = %q, want %q", sign.Artifacts, "checksums")
	}
	if sign.Signature != "${artifact}.sig" {
		t.Errorf("signs[0].signature = %q, want %q", sign.Signature, "${artifact}.sig")
	}
	// The signing command must reference the sign-checksums script so the
	// signature is produced with ed25519, not an unsigned or GPG default.
	found := false
	for _, arg := range sign.Args {
		if arg == "./scripts/sign-checksums" {
			found = true
		}
	}
	if !found {
		t.Errorf("signs[0].args does not reference ./scripts/sign-checksums: %v", sign.Args)
	}
}

// TestSecurityPinnedKeyConsistency asserts that the ed25519 public key
// pinned in updater.go matches the key embedded in
// scripts/verify-checksums/main.go. If they diverge, manual signature
// verification would accept signatures the updater rejects (or vice versa).
func TestSecurityPinnedKeyConsistency(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join(root, "scripts", "verify-checksums", "main.go"))
	if err != nil {
		t.Fatalf("read scripts/verify-checksums/main.go: %v", err)
	}
	// Extract the hex key from: const pinnedKey = "<hex>"
	re := regexp.MustCompile(`const pinnedKey = "([0-9a-fA-F]{64})"`)
	m := re.FindStringSubmatch(string(script))
	if m == nil {
		t.Fatal("scripts/verify-checksums/main.go: no pinnedKey const with a 64-char hex value found")
	}
	scriptKey, err := hex.DecodeString(m[1])
	if err != nil {
		t.Fatalf("decode script key: %v", err)
	}
	if !bytes.Equal(pinnedReleaseSigningKey, scriptKey) {
		t.Errorf("pinned key mismatch: updater.go has %x, verify-checksums has %x",
			[]byte(pinnedReleaseSigningKey), scriptKey)
	}
}

// TestSecurityVerifyReleaseSignatureRoundTrip asserts that
// verifyReleaseSignature accepts a valid ed25519 signature and rejects a
// tampered one. The pinned key is temporarily swapped for a test keypair
// so the test does not need the release private key.
func TestSecurityVerifyReleaseSignatureRoundTrip(t *testing.T) {
	orig := pinnedReleaseSigningKey
	defer func() { pinnedReleaseSigningKey = orig }()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pinnedReleaseSigningKey = pub

	data := []byte("checksums content")
	sig := ed25519.Sign(priv, data)

	if err := verifyReleaseSignature(data, sig); err != nil {
		t.Fatalf("verifyReleaseSignature rejected valid signature: %v", err)
	}

	// Tamper the data: the signature must no longer verify.
	if err := verifyReleaseSignature([]byte("tampered"), sig); err == nil {
		t.Fatal("verifyReleaseSignature accepted a signature over different data")
	}

	// Wrong key: a signature from a different keypair must be rejected.
	_, priv2, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey (2): %v", err)
	}
	sig2 := ed25519.Sign(priv2, data)
	if err := verifyReleaseSignature(data, sig2); err == nil {
		t.Fatal("verifyReleaseSignature accepted a signature from a different key")
	}

	// Wrong length: must be rejected with a clear error.
	if err := verifyReleaseSignature(data, []byte("short")); err == nil {
		t.Fatal("verifyReleaseSignature accepted a too-short signature")
	}
}
