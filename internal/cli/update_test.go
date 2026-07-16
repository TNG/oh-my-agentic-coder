package cli

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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/updater"
)

// --- fakes implementing updater's exported interfaces -----------------

type fakeSource struct {
	rel updater.Release
	err error
}

func (f fakeSource) LatestRelease(ctx context.Context) (updater.Release, error) {
	return f.rel, f.err
}

type fakeFetcher struct {
	files map[string][]byte
}

func (f fakeFetcher) FetchAll(ctx context.Context, url string) ([]byte, error) {
	body, ok := f.files[url]
	if !ok {
		return nil, fmt.Errorf("fakeFetcher: no fixture for %s", url)
	}
	return body, nil
}

func (f fakeFetcher) FetchToFile(ctx context.Context, url, dir, pattern string) (string, error) {
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
	return tmp.Name(), nil
}

type fakeRunner struct{ err error }

func (f fakeRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return f.err
}

type fakeReplacer struct{ err error }

func (f fakeReplacer) Replace(path string, r io.Reader, mode fs.FileMode) error {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	return f.err
}

func checksumsFile(name string, content []byte) []byte {
	sum := sha256.Sum256(content)
	return []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name))
}

func baseTestDeps(t *testing.T) updater.Deps {
	t.Helper()
	return updater.Deps{
		Executable: func() (string, error) { return filepath.Join(t.TempDir(), "omac"), nil },
		TempDir:    t.TempDir(),
		GOOS:       "linux",
		GOARCH:     "amd64",
		Stdin:      bytes.NewReader(nil),
		Stdout:     io.Discard,
		Stderr:     io.Discard,
	}
}

// --- runUpdateWithDeps tests --------------------------------------------

func runUpdateCapture(t *testing.T, yes bool, deps updater.Deps, stdinContent string) (stdout, stderr string, code int) {
	t.Helper()
	dir := t.TempDir()
	outF, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	errF, err := os.Create(filepath.Join(dir, "err"))
	if err != nil {
		t.Fatal(err)
	}
	inPath := filepath.Join(dir, "in")
	if err := os.WriteFile(inPath, []byte(stdinContent), 0o644); err != nil {
		t.Fatal(err)
	}
	inF, err := os.Open(inPath)
	if err != nil {
		t.Fatal(err)
	}
	env := &Env{Version: "1.0.0", Workdir: dir, Stdout: outF, Stderr: errF, Stdin: inF}

	code = runUpdateWithDeps(env, yes, deps)

	outF.Close()
	errF.Close()
	inF.Close()
	o, _ := os.ReadFile(outF.Name())
	e, _ := os.ReadFile(errF.Name())
	return string(o), string(e), code
}

func TestRunUpdate_AlreadyUpToDate(t *testing.T) {
	deps := baseTestDeps(t)
	deps.Source = fakeSource{rel: updater.Release{TagName: "v1.0.0"}}
	deps.Fetcher = fakeFetcher{files: map[string][]byte{}}

	out, _, code := runUpdateCapture(t, false, deps, "")
	if code != ExitOK {
		t.Fatalf("code = %d, want ExitOK; out=%s", code, out)
	}
	if !strings.Contains(out, "already up to date") {
		t.Fatalf("out = %q, want mention of already up to date", out)
	}
}

func TestRunUpdate_CurrentNewerThanLatestNoDowngrade(t *testing.T) {
	deps := baseTestDeps(t)
	// runUpdateCapture pins env.Version to 1.0.0; the latest release is older.
	deps.Source = fakeSource{rel: updater.Release{TagName: "v0.9.0"}}
	deps.Fetcher = fakeFetcher{files: map[string][]byte{}}
	deps.Runner = fakeRunner{}

	out, _, code := runUpdateCapture(t, true, deps, "")
	if code != ExitOK {
		t.Fatalf("code = %d, want ExitOK; out=%s", code, out)
	}
	if !strings.Contains(out, "newer than the latest release") {
		t.Fatalf("out = %q, want a 'newer than the latest release' notice", out)
	}
	if strings.Contains(out, "updated omac") {
		t.Fatalf("out = %q, must not downgrade to an older release", out)
	}
}

func TestRunUpdate_NonInteractiveWithoutYesNoops(t *testing.T) {
	debURL := "https://example.invalid/x.deb"
	sumsURL := "https://example.invalid/checksums.txt"
	deps := baseTestDeps(t)
	deps.Source = fakeSource{rel: updater.Release{TagName: "v2.0.0", Assets: []updater.Asset{
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.deb", BrowserDownloadURL: debURL},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	body := []byte("deb-bytes")
	deps.Fetcher = fakeFetcher{files: map[string][]byte{
		sumsURL: checksumsFile("oh-my-agentic-coder_2.0.0_linux_x86_64.deb", body),
		debURL:  body,
	}}
	deps.PkgManagers = []updater.PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}
	deps.Runner = fakeRunner{}

	// Regular file stdin is never a terminal, so this exercises the
	// non-interactive branch regardless of content.
	out, _, code := runUpdateCapture(t, false, deps, "y\n")
	if code != ExitOK {
		t.Fatalf("code = %d, want ExitOK; out=%s", code, out)
	}
	if !strings.Contains(out, "[noop]") {
		t.Fatalf("out = %q, want a [noop] hint", out)
	}
	if strings.Contains(out, "updated omac") {
		t.Fatalf("out = %q, should not have applied the update", out)
	}
}

func TestRunUpdate_YesSkipsPromptAndApplies(t *testing.T) {
	debURL := "https://example.invalid/x.deb"
	sumsURL := "https://example.invalid/checksums.txt"
	deps := baseTestDeps(t)
	deps.Source = fakeSource{rel: updater.Release{TagName: "v2.0.0", Assets: []updater.Asset{
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.deb", BrowserDownloadURL: debURL},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	body := []byte("deb-bytes")
	deps.Fetcher = fakeFetcher{files: map[string][]byte{
		sumsURL: checksumsFile("oh-my-agentic-coder_2.0.0_linux_x86_64.deb", body),
		debURL:  body,
	}}
	deps.PkgManagers = []updater.PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}
	deps.Runner = fakeRunner{}

	out, _, code := runUpdateCapture(t, true, deps, "")
	if code != ExitOK {
		t.Fatalf("code = %d, want ExitOK; out=%s", code, out)
	}
	if !strings.Contains(out, "updated omac 1.0.0 -> 2.0.0") {
		t.Fatalf("out = %q, want update confirmation", out)
	}
}

func TestRunUpdate_ChecksumMismatch(t *testing.T) {
	debURL := "https://example.invalid/x.deb"
	sumsURL := "https://example.invalid/checksums.txt"
	deps := baseTestDeps(t)
	deps.Source = fakeSource{rel: updater.Release{TagName: "v2.0.0", Assets: []updater.Asset{
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.deb", BrowserDownloadURL: debURL},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	deps.Fetcher = fakeFetcher{files: map[string][]byte{
		sumsURL: checksumsFile("oh-my-agentic-coder_2.0.0_linux_x86_64.deb", []byte("expected")),
		debURL:  []byte("actually-different"),
	}}
	deps.PkgManagers = []updater.PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}

	_, errOut, code := runUpdateCapture(t, true, deps, "")
	if code != ExitChecksumMismatch {
		t.Fatalf("code = %d, want ExitChecksumMismatch; stderr=%s", code, errOut)
	}
}

func TestRunUpdate_NoMatchingAsset(t *testing.T) {
	deps := baseTestDeps(t)
	deps.Source = fakeSource{rel: updater.Release{TagName: "v2.0.0", Assets: []updater.Asset{
		{Name: "oh-my-agentic-coder_2.0.0_linux_arm64.deb", BrowserDownloadURL: "https://example.invalid/x.deb"},
	}}}
	deps.Fetcher = fakeFetcher{files: map[string][]byte{}}
	deps.PkgManagers = []updater.PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}

	_, _, code := runUpdateCapture(t, true, deps, "")
	if code != ExitPrerequisiteMissing {
		t.Fatalf("code = %d, want ExitPrerequisiteMissing", code)
	}
}

func TestRunUpdate_InstallCommandFailure(t *testing.T) {
	debURL := "https://example.invalid/x.deb"
	sumsURL := "https://example.invalid/checksums.txt"
	deps := baseTestDeps(t)
	deps.Source = fakeSource{rel: updater.Release{TagName: "v2.0.0", Assets: []updater.Asset{
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.deb", BrowserDownloadURL: debURL},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	body := []byte("deb-bytes")
	deps.Fetcher = fakeFetcher{files: map[string][]byte{
		sumsURL: checksumsFile("oh-my-agentic-coder_2.0.0_linux_x86_64.deb", body),
		debURL:  body,
	}}
	deps.PkgManagers = []updater.PackageManager{
		{Name: "dpkg", AssetSuffix: ".deb", InstallArgs: func(p string) []string { return []string{"-i", p} }},
	}
	deps.Runner = fakeRunner{err: fakeExitError(t)}

	_, errOut, code := runUpdateCapture(t, true, deps, "")
	if code != ExitGeneric {
		t.Fatalf("code = %d, want ExitGeneric; stderr=%s", code, errOut)
	}
}

func TestRunUpdate_SelfReplacePermissionDenied(t *testing.T) {
	dir := t.TempDir()
	tgzPath := filepath.Join(dir, "asset.tar.gz")
	writeTestTarGzForCLI(t, tgzPath, "omac", []byte("bytes"), 0o755)
	tgzURL := "https://example.invalid/x.tar.gz"
	sumsURL := "https://example.invalid/checksums.txt"

	deps := baseTestDeps(t)
	deps.GOOS, deps.GOARCH = "linux", "amd64"
	deps.Source = fakeSource{rel: updater.Release{TagName: "v2.0.0", Assets: []updater.Asset{
		{Name: "oh-my-agentic-coder_2.0.0_linux_x86_64.tar.gz", BrowserDownloadURL: tgzURL},
		{Name: "checksums.txt", BrowserDownloadURL: sumsURL},
	}}}
	content, err := os.ReadFile(tgzPath)
	if err != nil {
		t.Fatal(err)
	}
	deps.Fetcher = fakeFetcher{files: map[string][]byte{
		sumsURL: checksumsFile("oh-my-agentic-coder_2.0.0_linux_x86_64.tar.gz", content),
		tgzURL:  content,
	}}
	deps.PkgManagers = nil
	deps.Replacer = fakeReplacer{err: fmt.Errorf("wrap: %w", fs.ErrPermission)}

	_, errOut, code := runUpdateCapture(t, true, deps, "")
	if code != ExitIOError {
		t.Fatalf("code = %d, want ExitIOError; stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "permission denied") {
		t.Fatalf("stderr = %q, want a permission-denied hint", errOut)
	}
}

func fakeExitError(t *testing.T) *exec.ExitError {
	t.Helper()
	err := exec.Command("false").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError from `false`, got %v", err)
	}
	return exitErr
}

func writeTestTarGzForCLI(t *testing.T, path, name string, content []byte, mode int64) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content))}); err != nil {
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

// --- parseYesNo ------------------------------------------------------------

func TestParseYesNo(t *testing.T) {
	cases := map[string]bool{
		"y\n":   true,
		"yes\n": true,
		"Y\n":   true,
		"n\n":   false,
		"":      false,
		"nope":  false,
	}
	for input, want := range cases {
		if got := parseYesNo(input); got != want {
			t.Errorf("parseYesNo(%q) = %v, want %v", input, got, want)
		}
	}
}
