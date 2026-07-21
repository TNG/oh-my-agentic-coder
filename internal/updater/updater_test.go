package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// --- fakes ---------------------------------------------------------------

type fakeReleaseSource struct {
	rel Release
	err error
}

func (f fakeReleaseSource) LatestRelease(ctx context.Context) (Release, error) {
	return f.rel, f.err
}

// fakeFetcher serves fixed content per URL and never touches the network.
type fakeFetcher struct {
	files map[string][]byte // url -> body
	calls int
	paths []string // temp files created by FetchToFile
}

func (f *fakeFetcher) FetchAll(ctx context.Context, url string) ([]byte, error) {
	f.calls++
	body, ok := f.files[url]
	if !ok {
		return nil, fmt.Errorf("fakeFetcher: no fixture for %s", url)
	}
	return body, nil
}

func (f *fakeFetcher) FetchToFile(ctx context.Context, url, dir, pattern string) (string, error) {
	f.calls++
	body, ok := f.files[url]
	if !ok {
		return "", fmt.Errorf("fakeFetcher: no fixture for %s", url)
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

type runCall struct {
	name string
	args []string
}

type fakeRunner struct {
	calls []runCall
	err   error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	f.calls = append(f.calls, runCall{name: name, args: args})
	return f.err
}

type fakeReplacer struct {
	err  error
	got  []byte
	path string
	mode fs.FileMode
}

func (f *fakeReplacer) Replace(path string, r io.Reader, mode fs.FileMode) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.path, f.got, f.mode = path, data, mode
	if f.err != nil {
		return f.err
	}
	return nil
}

func checksumsFile(name string, content []byte) []byte {
	sum := sha256.Sum256(content)
	return []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name))
}

func baseDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		Executable: func() (string, error) { return filepath.Join(t.TempDir(), "omac"), nil },
		TempDir:    t.TempDir(),
		Stdin:      bytes.NewReader(nil),
		Stdout:     io.Discard,
		Stderr:     io.Discard,
	}
}

// --- Check() tests ---------------------------------------------------------

func TestCheck_AlreadyUpToDate(t *testing.T) {
	deps := baseDeps(t)
	deps.Source = fakeReleaseSource{rel: Release{TagName: "v1.2.3"}}
	deps.Fetcher = &fakeFetcher{files: map[string][]byte{}}
	deps.GOOS, deps.GOARCH = "linux", "amd64"

	plan, err := Check(context.Background(), Options{CurrentVersion: "1.2.3"}, deps)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if plan.Method != MethodUpToDate {
		t.Fatalf("Method = %v, want MethodUpToDate", plan.Method)
	}
}

func TestCheck_CurrentNewerThanLatestIsUpToDate(t *testing.T) {
	deps := baseDeps(t)
	// A dev/hotfix build ahead of the latest published release: with a package
	// manager present and matching assets available, the only thing stopping an
	// install is the version guard — so this proves it never downgrades.
	deps.Source = fakeReleaseSource{rel: Release{TagName: "v1.2.0", Assets: []Asset{
		{Name: "oh-my-agentic-coder_1.2.0_linux_x86_64.deb", BrowserDownloadURL: "https://example.invalid/x.deb"},
		{Name: "checksums.txt", BrowserDownloadURL: "https://example.invalid/checksums.txt"},
	}}}
	fetch := &fakeFetcher{files: map[string][]byte{}}
	deps.Fetcher = fetch
	deps.GOOS, deps.GOARCH = "linux", "amd64"
	deps.PkgManagers = []PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}

	plan, err := Check(context.Background(), Options{CurrentVersion: "1.3.0"}, deps)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if plan.Method != MethodUpToDate {
		t.Fatalf("Method = %v, want MethodUpToDate (no downgrade)", plan.Method)
	}
	if fetch.calls != 0 {
		t.Fatalf("expected zero downloads when already newer, got %d", fetch.calls)
	}
}

func TestCheck_DevBuildUpdatesToRelease(t *testing.T) {
	tgzBody := []byte("fake-tar-gz-bytes")
	sumsURL := "https://example.invalid/checksums.txt"
	tgzURL := "https://example.invalid/oh-my-agentic-coder_0.1.0_linux_x86_64.tar.gz"

	deps := baseDeps(t)
	deps.Source = fakeReleaseSource{rel: Release{TagName: "v0.1.0", Assets: []Asset{
		{Name: "oh-my-agentic-coder_0.1.0_linux_x86_64.tar.gz", BrowserDownloadURL: tgzURL},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	deps.Fetcher = &fakeFetcher{files: map[string][]byte{
		sumsURL: checksumsFile("oh-my-agentic-coder_0.1.0_linux_x86_64.tar.gz", tgzBody),
		tgzURL:  tgzBody,
	}}
	deps.GOOS, deps.GOARCH = "linux", "amd64"

	// A pre-release build of the same core version installs the release.
	plan, err := Check(context.Background(), Options{CurrentVersion: "0.1.0-dev"}, deps)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if plan.Method != MethodTarballSelfReplace {
		t.Fatalf("Method = %v, want MethodTarballSelfReplace", plan.Method)
	}
}

func TestCheck_DarwinBrewInstalled(t *testing.T) {
	deps := baseDeps(t)
	deps.Source = fakeReleaseSource{rel: Release{TagName: "v2.0.0"}}
	fetch := &fakeFetcher{files: map[string][]byte{}}
	deps.Fetcher = fetch
	deps.GOOS, deps.GOARCH = "darwin", "arm64"
	deps.BrewInstalled = func() bool { return true }

	plan, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if plan.Method != MethodBrew {
		t.Fatalf("Method = %v, want MethodBrew", plan.Method)
	}
	if fetch.calls != 0 {
		t.Fatalf("expected zero downloads for brew method, got %d calls", fetch.calls)
	}
}

func TestCheck_LinuxPackageManagerPriority(t *testing.T) {
	debBody := []byte("deb-bytes")
	sumsURL := "https://example.invalid/checksums.txt"
	debURL := "https://example.invalid/oh-my-agentic-coder_2.0.0_linux_x86_64.deb"

	deps := baseDeps(t)
	deps.Source = fakeReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.deb", BrowserDownloadURL: debURL},
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.rpm", BrowserDownloadURL: "https://example.invalid/x.rpm"},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	deps.Fetcher = &fakeFetcher{files: map[string][]byte{
		sumsURL: checksumsFile("oh-my-agentic-coder_2.0.0_linux_x86_64.deb", debBody),
		debURL:  debBody,
	}}
	deps.GOOS, deps.GOARCH = "linux", "amd64"
	deps.PkgManagers = []PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
		{Name: "rpm", AssetSuffix: ".rpm", InstallArgs: func(p string) []string { return []string{"-U", p} }},
	}

	plan, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if plan.Method != MethodLinuxPackage || plan.PackageManager != "dpkg" {
		t.Fatalf("plan = %+v, want MethodLinuxPackage/dpkg", plan)
	}
	if !plan.ChecksumVerified {
		t.Fatalf("expected checksum verified")
	}
}

func TestCheck_PrefersSelfReplaceWhenBinaryWritable(t *testing.T) {
	tgzBody := []byte("fake-tar-gz-bytes")
	sumsURL := "https://example.invalid/checksums.txt"
	tgzURL := "https://example.invalid/oh-my-agentic-coder_2.0.0_linux_x86_64.tar.gz"
	debURL := "https://example.invalid/oh-my-agentic-coder_2.0.0_linux_x86_64.deb"

	deps := baseDeps(t)
	deps.Source = fakeReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.deb", BrowserDownloadURL: debURL},
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.tar.gz", BrowserDownloadURL: tgzURL},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	deps.Fetcher = &fakeFetcher{files: map[string][]byte{
		sumsURL: checksumsFile("oh-my-agentic-coder_2.0.0_linux_x86_64.tar.gz", tgzBody),
		tgzURL:  tgzBody,
	}}
	deps.GOOS, deps.GOARCH = "linux", "amd64"
	// A package manager is present, but the running binary is user-writable,
	// so Check must prefer the no-sudo self-replace over the package manager.
	deps.PkgManagers = []PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}
	deps.SelfReplaceable = func(string) bool { return true }

	plan, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if plan.Method != MethodTarballSelfReplace {
		t.Fatalf("Method = %v, want MethodTarballSelfReplace (writable binary → no sudo)", plan.Method)
	}
	if !plan.ChecksumVerified {
		t.Fatalf("expected checksum verified")
	}
}

func TestCheck_UsesPackageManagerWhenBinaryNotWritable(t *testing.T) {
	debBody := []byte("deb-bytes")
	sumsURL := "https://example.invalid/checksums.txt"
	debURL := "https://example.invalid/oh-my-agentic-coder_2.0.0_linux_x86_64.deb"

	deps := baseDeps(t)
	deps.Source = fakeReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.deb", BrowserDownloadURL: debURL},
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.tar.gz", BrowserDownloadURL: "https://example.invalid/x.tar.gz"},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	deps.Fetcher = &fakeFetcher{files: map[string][]byte{
		sumsURL: checksumsFile("oh-my-agentic-coder_2.0.0_linux_x86_64.deb", debBody),
		debURL:  debBody,
	}}
	deps.GOOS, deps.GOARCH = "linux", "amd64"
	deps.PkgManagers = []PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}
	// Root-owned install: self-replace would need elevation, so keep the
	// package manager even though a tarball asset exists.
	deps.SelfReplaceable = func(string) bool { return false }

	plan, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if plan.Method != MethodLinuxPackage || plan.PackageManager != "dpkg" {
		t.Fatalf("plan = %+v, want MethodLinuxPackage/dpkg", plan)
	}
}

func TestCheck_LinuxNoPackageManagerFallsBackToTarball(t *testing.T) {
	tgzBody := []byte("fake-tar-gz-bytes")
	sumsURL := "https://example.invalid/checksums.txt"
	tgzURL := "https://example.invalid/oh-my-agentic-coder_2.0.0_linux_x86_64.tar.gz"

	deps := baseDeps(t)
	deps.Source = fakeReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.tar.gz", BrowserDownloadURL: tgzURL},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	deps.Fetcher = &fakeFetcher{files: map[string][]byte{
		sumsURL: checksumsFile("oh-my-agentic-coder_2.0.0_linux_x86_64.tar.gz", tgzBody),
		tgzURL:  tgzBody,
	}}
	deps.GOOS, deps.GOARCH = "linux", "amd64"
	deps.PkgManagers = nil

	plan, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if plan.Method != MethodTarballSelfReplace {
		t.Fatalf("Method = %v, want MethodTarballSelfReplace", plan.Method)
	}
}

func TestCheck_ChecksumMismatch(t *testing.T) {
	sumsURL := "https://example.invalid/checksums.txt"
	debURL := "https://example.invalid/oh-my-agentic-coder_2.0.0_linux_x86_64.deb"

	deps := baseDeps(t)
	deps.Source = fakeReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.deb", BrowserDownloadURL: debURL},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	deps.Fetcher = &fakeFetcher{files: map[string][]byte{
		sumsURL: checksumsFile("oh-my-agentic-coder_2.0.0_linux_x86_64.deb", []byte("expected-bytes")),
		debURL:  []byte("actually-different-bytes"),
	}}
	deps.GOOS, deps.GOARCH = "linux", "amd64"
	deps.PkgManagers = []PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}

	_, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
}

func TestCheck_ChecksumMismatch_CleansTempFile(t *testing.T) {
	sumsURL := "https://example.invalid/checksums.txt"
	debURL := "https://example.invalid/oh-my-agentic-coder_2.0.0_linux_x86_64.deb"

	deps := baseDeps(t)
	deps.Source = fakeReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.deb", BrowserDownloadURL: debURL},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	fetcher := &fakeFetcher{files: map[string][]byte{
		sumsURL: checksumsFile("oh-my-agentic-coder_2.0.0_linux_x86_64.deb", []byte("expected-bytes")),
		debURL:  []byte("actually-different-bytes"),
	}}
	deps.Fetcher = fetcher
	deps.GOOS, deps.GOARCH = "linux", "amd64"
	deps.PkgManagers = []PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}

	_, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	if len(fetcher.paths) != 1 {
		t.Fatalf("expected 1 temp file created, got %d", len(fetcher.paths))
	}
	if _, statErr := os.Stat(fetcher.paths[0]); !os.IsNotExist(statErr) {
		t.Fatalf("temp file %q still exists after checksum mismatch: %v", fetcher.paths[0], statErr)
	}
}

func TestCheck_NoMatchingAsset(t *testing.T) {
	deps := baseDeps(t)
	deps.Source = fakeReleaseSource{rel: Release{TagName: "v2.0.0", Assets: []Asset{
		{Name: "oh-my-agentic-coder_2.0.0_linux_arm64.deb", BrowserDownloadURL: "https://example.invalid/x.deb"},
	}}}
	deps.Fetcher = &fakeFetcher{files: map[string][]byte{}}
	deps.GOOS, deps.GOARCH = "linux", "amd64"
	deps.PkgManagers = []PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}

	_, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
	if !errors.Is(err, ErrNoMatchingAsset) {
		t.Fatalf("err = %v, want ErrNoMatchingAsset", err)
	}
}

// --- Apply() tests ---------------------------------------------------------

func TestApply_LinuxPackage_RunsSudoWithPackageManager(t *testing.T) {
	deps := baseDeps(t)
	runner := &fakeRunner{}
	deps.Runner = runner
	deps.PkgManagers = []PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}
	plan := Plan{Method: MethodLinuxPackage, PackageManager: "dpkg", LocalPath: "/tmp/x.deb"}

	if err := Apply(context.Background(), plan, deps); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 run call, got %d", len(runner.calls))
	}
	got := runner.calls[0]
	if got.name != "sudo" {
		t.Fatalf("name = %q, want sudo", got.name)
	}
	want := []string{"dpkg", "-i", "/tmp/x.deb"}
	if len(got.args) != len(want) {
		t.Fatalf("args = %v, want %v", got.args, want)
	}
	for i := range want {
		if got.args[i] != want[i] {
			t.Fatalf("args = %v, want %v", got.args, want)
		}
	}
}

func TestApply_Brew(t *testing.T) {
	deps := baseDeps(t)
	runner := &fakeRunner{}
	deps.Runner = runner
	plan := Plan{Method: MethodBrew}

	if err := Apply(context.Background(), plan, deps); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "brew" {
		t.Fatalf("calls = %+v, want one brew call", runner.calls)
	}
}

func TestApply_InstallCommandFailure(t *testing.T) {
	deps := baseDeps(t)
	sentinel := errors.New("dpkg exited 1")
	deps.Runner = &fakeRunner{err: sentinel}
	deps.PkgManagers = []PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}
	plan := Plan{Method: MethodLinuxPackage, PackageManager: "dpkg", LocalPath: "/tmp/x.deb"}

	err := Apply(context.Background(), plan, deps)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel wrapped", err)
	}
}

func TestApply_SelfReplace_ExtractsAndWritesBinary(t *testing.T) {
	dir := t.TempDir()
	tgzPath := filepath.Join(dir, "asset.tar.gz")
	writeTestTarGz(t, tgzPath, "omac", []byte("new-binary-bytes"), 0o755)

	deps := baseDeps(t)
	replacer := &fakeReplacer{}
	deps.Replacer = replacer
	deps.Executable = func() (string, error) { return filepath.Join(dir, "omac"), nil }
	plan := Plan{Method: MethodTarballSelfReplace, LocalPath: tgzPath}

	if err := Apply(context.Background(), plan, deps); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if string(replacer.got) != "new-binary-bytes" {
		t.Fatalf("replacer got %q", replacer.got)
	}
	if replacer.mode != 0o755 {
		t.Fatalf("mode = %v, want 0755", replacer.mode)
	}
}

func TestApply_SelfReplace_PermissionDenied(t *testing.T) {
	dir := t.TempDir()
	tgzPath := filepath.Join(dir, "asset.tar.gz")
	writeTestTarGz(t, tgzPath, "omac", []byte("bytes"), 0o755)

	deps := baseDeps(t)
	deps.Replacer = &fakeReplacer{err: fmt.Errorf("wrap: %w", fs.ErrPermission)}
	deps.Executable = func() (string, error) { return filepath.Join(dir, "omac"), nil }
	plan := Plan{Method: MethodTarballSelfReplace, LocalPath: tgzPath}

	err := Apply(context.Background(), plan, deps)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want fs.ErrPermission", err)
	}
}

func TestSelfReplaceable(t *testing.T) {
	dir := t.TempDir()
	if !selfReplaceable(filepath.Join(dir, "omac")) {
		t.Fatalf("selfReplaceable = false for a writable temp dir, want true")
	}
	if selfReplaceable(filepath.Join(dir, "no-such-subdir", "omac")) {
		t.Fatalf("selfReplaceable = true for a non-existent directory, want false")
	}
}

func TestRunningBinary_FollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "omac-real")
	if err := os.WriteFile(real, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "omac-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	deps := Deps{Executable: func() (string, error) { return link, nil }}
	got, err := runningBinary(deps)
	if err != nil {
		t.Fatalf("runningBinary: %v", err)
	}
	if got != real {
		t.Fatalf("runningBinary = %q, want resolved %q", got, real)
	}
}

// --- parseYesNo-style pure helpers (parseChecksums) ------------------------

func TestParseChecksums(t *testing.T) {
	data := []byte("abc123  file-a.deb\ndeadbeef  file-b.tar.gz\n")
	sum, ok := parseChecksums(data, "file-b.tar.gz")
	if !ok || sum != "deadbeef" {
		t.Fatalf("sum=%q ok=%v, want deadbeef/true", sum, ok)
	}
	if _, ok := parseChecksums(data, "missing.deb"); ok {
		t.Fatalf("expected no match for missing file")
	}
}
