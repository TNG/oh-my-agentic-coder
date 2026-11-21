package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type securityStaticReleaseSource struct {
	rel Release
}

func (s securityStaticReleaseSource) LatestRelease(context.Context) (Release, error) {
	return s.rel, nil
}

func securityReleaseServer(t *testing.T, assetName string, assetBody []byte) Release {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/asset":
			_, _ = w.Write(assetBody)
		case "/checksums.txt":
			sum := sha256.Sum256(assetBody)
			_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	return Release{
		TagName: "v2.0.0",
		Assets: []Asset{
			{Name: assetName, BrowserDownloadURL: srv.URL + "/asset"},
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums.txt"},
		},
	}
}

func securityTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(content)),
	}); err != nil {
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
	return buf.Bytes()
}

func writeSecuritySudoStub(t *testing.T, dir string) {
	t.Helper()

	script := `#!/bin/sh
target=
for arg in "$@"; do
	case "$arg" in
	/proc/self/fd/*|/dev/fd/*) target=$arg ;;
	esac
done
if [ -z "$target" ]; then
	for arg in "$@"; do
		if [ -f "$arg" ]; then target=$arg; fi
	done
fi
if [ -z "$target" ]; then
	exit 1
fi
cat "$target" > "$SECURITY_CAPTURE" || exit 1
printf '%s\n' "$@" > "$SECURITY_ARGS"
`
	path := filepath.Join(dir, "sudo")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write sudo stub: %v", err)
	}
}

func securityHasDescriptorArg(args []byte) bool {
	a := string(args)
	return strings.Contains(a, "/proc/self/fd/") || strings.Contains(a, "/dev/fd/")
}

// securityDownloadedArtifacts returns whatever Check left in the configured
// temp directory. A fixed implementation may hold the verified bytes in memory
// or in a descriptor and leave no path behind, so callers must tolerate an
// empty result.
func securityDownloadedArtifacts(t *testing.T, dir string) []string {
	t.Helper()

	var paths []string
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("scan temp dir %q: %v", dir, err)
	}
	return paths
}

// TestSecurityUpdateBindsArtifactToVerifiedDescriptor asserts that the package
// manager receives the artifact whose digest was verified, even if the download
// path is reused before Apply.
func TestSecurityUpdateBindsArtifactToVerifiedDescriptor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor inheritance is Linux-specific")
	}

	managers := []struct {
		name   string
		suffix string
	}{
		{name: "dpkg", suffix: ".deb"},
		{name: "rpm", suffix: ".rpm"},
		{name: "pacman", suffix: ".pkg.tar.zst"},
		{name: "apk", suffix: ".apk"},
	}

	for _, pm := range managers {
		t.Run(pm.name, func(t *testing.T) {
			verified := []byte("verified package artifact")
			replacement := []byte("replacement package artifact")
			assetName := "oh-my-agentic-coder_2.0.0_linux_x86_64" + pm.suffix
			rel := securityReleaseServer(t, assetName, verified)

			deps := RealDeps(bytes.NewReader(nil), io.Discard, io.Discard)
			deps.Source = securityStaticReleaseSource{rel: rel}
			deps.GOOS, deps.GOARCH = "linux", "amd64"
			deps.TempDir = t.TempDir()
			deps.SelfReplaceable = func(string) bool { return false }
			deps.PkgManagers = []PackageManager{{
				Name:        pm.name,
				AssetSuffix: pm.suffix,
				InstallArgs: func(p string) []string { return []string{p} },
			}}

			binDir := t.TempDir()
			writeSecuritySudoStub(t, binDir)
			capturePath := filepath.Join(t.TempDir(), "capture")
			argsPath := filepath.Join(t.TempDir(), "args")
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("SECURITY_CAPTURE", capturePath)
			t.Setenv("SECURITY_ARGS", argsPath)

			plan, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}

			artifacts := securityDownloadedArtifacts(t, deps.TempDir)
			for _, path := range artifacts {
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatalf("make download writable %q: %v", path, err)
				}
				if err := os.WriteFile(path, replacement, 0o600); err != nil {
					t.Fatalf("replace download %q: %v", path, err)
				}
			}

			if err := Apply(context.Background(), plan, deps); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			got, err := os.ReadFile(capturePath)
			if err != nil {
				t.Fatalf("installer did not receive an artifact: %v", err)
			}
			if !bytes.Equal(got, verified) {
				t.Fatalf("installer received %q, want the verified artifact %q", got, verified)
			}

			args, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatalf("read installer arguments: %v", err)
			}
			if !securityHasDescriptorArg(args) {
				t.Fatalf("installer arguments %q did not include an inherited descriptor path", args)
			}
			for _, path := range artifacts {
				if strings.Contains(string(args), path) {
					t.Fatalf("installer arguments %q include the mutable download path %q", args, path)
				}
			}
		})
	}
}

// TestSecurityUpdateRejectsPathSwapBeforeApply asserts that reusing the
// download path after verification either leaves the verified bytes in place
// or fails closed; the replacement bytes must never be installed.
func TestSecurityUpdateRejectsPathSwapBeforeApply(t *testing.T) {
	cases := []struct {
		name string
		swap func(t *testing.T, path string, replacement []byte)
	}{
		{
			name: "overwritten",
			swap: func(t *testing.T, path string, replacement []byte) {
				t.Helper()
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatalf("make download writable %q: %v", path, err)
				}
				if err := os.WriteFile(path, replacement, 0o600); err != nil {
					t.Fatalf("overwrite download %q: %v", path, err)
				}
			},
		},
		{
			name: "symlink",
			swap: func(t *testing.T, path string, replacement []byte) {
				t.Helper()
				replacementPath := filepath.Join(t.TempDir(), "replacement.tar.gz")
				if err := os.WriteFile(replacementPath, replacement, 0o600); err != nil {
					t.Fatalf("write replacement archive: %v", err)
				}
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					t.Fatalf("remove download %q: %v", path, err)
				}
				if err := os.Symlink(replacementPath, path); err != nil {
					t.Fatalf("link replacement at %q: %v", path, err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verifiedBinary := []byte("verified binary artifact")
			replacementBinary := []byte("replacement binary artifact")
			verifiedArchive := securityTarGz(t, "omac", verifiedBinary)
			replacementArchive := securityTarGz(t, "omac", replacementBinary)
			assetName := "oh-my-agentic-coder_2.0.0_linux_x86_64.tar.gz"
			rel := securityReleaseServer(t, assetName, verifiedArchive)

			target := filepath.Join(t.TempDir(), "omac-installed")
			if err := os.WriteFile(target, []byte("binary before update"), 0o755); err != nil {
				t.Fatalf("write installed binary: %v", err)
			}

			deps := RealDeps(bytes.NewReader(nil), io.Discard, io.Discard)
			deps.Source = securityStaticReleaseSource{rel: rel}
			deps.GOOS, deps.GOARCH = "linux", "amd64"
			deps.TempDir = t.TempDir()
			deps.Executable = func() (string, error) { return target, nil }
			deps.SelfReplaceable = func(string) bool { return true }
			deps.PkgManagers = nil

			plan, err := Check(context.Background(), Options{CurrentVersion: "1.0.0"}, deps)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}

			for _, path := range securityDownloadedArtifacts(t, deps.TempDir) {
				tc.swap(t, path, replacementArchive)
			}

			applyErr := Apply(context.Background(), plan, deps)
			installed, readErr := os.ReadFile(target)
			if readErr != nil {
				t.Fatalf("read installed binary: %v", readErr)
			}
			if bytes.Equal(installed, replacementBinary) {
				t.Fatalf("Apply installed the replacement artifact (Apply error: %v)", applyErr)
			}
			if applyErr == nil && !bytes.Equal(installed, verifiedBinary) {
				t.Fatalf("Apply succeeded but installed %q, want the verified artifact %q", installed, verifiedBinary)
			}
		})
	}
}
