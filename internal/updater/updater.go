// Package updater implements `omac update`: checking GitHub for the latest
// release, verifying its checksum, and installing it via the mechanism that
// matches how this host actually runs omac (a Linux package manager, brew,
// or a self-replace of the running binary).
//
// All real-world side effects (HTTP, subprocess exec, filesystem replace)
// are wrapped in interfaces on Deps, so Plan/Apply are fully testable
// without touching the network, sudo, or any real package manager.
package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Method identifies how Apply will install the update.
type Method int

const (
	MethodUpToDate Method = iota
	MethodBrew
	MethodLinuxPackage
	MethodTarballSelfReplace
)

// Asset is one file attached to a GitHub release.
type Asset struct {
	Name               string
	BrowserDownloadURL string
}

// Release is the subset of a GitHub release we need.
type Release struct {
	TagName string
	Assets  []Asset
}

// PackageManager describes one Linux package-manager installer.
type PackageManager struct {
	Name        string // "dpkg" | "rpm" | "pacman" | "apk"
	AssetSuffix string // ".deb" | ".rpm" | ".pkg.tar.zst" | ".apk"
	InstallArgs func(pkgPath string) []string
}

// Plan is the fully-resolved result of Plan(): what would happen, and (for
// every method except MethodUpToDate/MethodBrew) a checksum-verified local
// download ready for Apply to install.
type Plan struct {
	CurrentVersion   string
	LatestVersion    string
	Method           Method
	PackageManager   string // set for MethodLinuxPackage
	Asset            Asset
	LocalPath        string
	ChecksumVerified bool
}

// Options configures Plan.
type Options struct {
	// CurrentVersion is env.Version verbatim; Plan strips a leading "v"
	// before comparing against the latest release tag.
	CurrentVersion string
}

// Deps wraps every real-world side effect Plan/Apply need. RealDeps builds
// production wiring; tests build a Deps literal of fakes.
type Deps struct {
	Source   ReleaseSource
	Fetcher  Fetcher
	Runner   CommandRunner
	Replacer SelfReplacer

	// BrewInstalled reports whether omac is a Homebrew-managed install.
	// Only consulted when GOOS == "darwin".
	BrewInstalled func() bool

	// PkgManagers is the priority-ordered list of Linux package managers
	// present on this host (highest priority first). Empty means none
	// detected, so Plan falls back to MethodTarballSelfReplace.
	PkgManagers []PackageManager

	// Executable resolves the path of the running binary (for self-replace).
	Executable func() (string, error)

	GOOS, GOARCH string
	TempDir      string

	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// RealDeps builds production Deps: real HTTP, real subprocess exec, real
// filesystem replace, real host detection.
func RealDeps(stdin io.Reader, stdout, stderr io.Writer) Deps {
	return Deps{
		Source:        NewGitHubReleaseSource(),
		Fetcher:       NewHTTPFetcher(),
		Runner:        execRunner{},
		Replacer:      fsSelfReplacer{},
		BrewInstalled: func() bool { return DetectBrewInstalled(os.Executable, exec.LookPath, runCommand) },
		PkgManagers:   DetectPackageManagers(exec.LookPath),
		Executable:    os.Executable,
		GOOS:          runtime.GOOS,
		GOARCH:        runtime.GOARCH,
		TempDir:       os.TempDir(),
		Stdin:         stdin,
		Stdout:        stdout,
		Stderr:        stderr,
	}
}

func runCommand(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

var (
	// ErrNoMatchingAsset means no release asset matches this host's OS,
	// architecture, and available install method.
	ErrNoMatchingAsset = errors.New("no release asset matches this OS/architecture")
	// ErrChecksumMismatch means the downloaded asset's SHA-256 does not
	// match the release's published checksums.txt.
	ErrChecksumMismatch = errors.New("checksum verification failed")
)

// Check contacts GitHub for the latest release, decides the install method
// for this host, and (unless already up to date, or installing via brew)
// downloads and checksum-verifies the matching asset, returning a Plan. It
// never installs anything — see Apply for that.
func Check(ctx context.Context, opts Options, deps Deps) (Plan, error) {
	rel, err := deps.Source.LatestRelease(ctx)
	if err != nil {
		return Plan{}, fmt.Errorf("fetch latest release: %w", err)
	}
	current := strings.TrimPrefix(opts.CurrentVersion, "v")
	latest := strings.TrimPrefix(rel.TagName, "v")

	p := Plan{CurrentVersion: current, LatestVersion: latest}
	// Install only when the latest release is strictly newer. If this build is
	// the same as, or ahead of, the latest release (a dev or pre-release build
	// built past the last tag), there is nothing newer to install — never
	// downgrade. When the versions are not comparable as semver, fall back to
	// the conservative string-equality check.
	if cmp, ok := compareVersions(current, latest); current == latest || (ok && cmp >= 0) {
		p.Method = MethodUpToDate
		return p, nil
	}

	if deps.GOOS == "darwin" && deps.BrewInstalled != nil && deps.BrewInstalled() {
		p.Method = MethodBrew
		return p, nil
	}

	if deps.GOOS == "linux" && len(deps.PkgManagers) > 0 {
		pm := deps.PkgManagers[0]
		asset, ok := matchAsset(rel.Assets, deps.GOOS, deps.GOARCH, pm.AssetSuffix)
		if !ok {
			return Plan{}, fmt.Errorf("%w: no %s asset for %s/%s", ErrNoMatchingAsset, pm.AssetSuffix, deps.GOOS, deps.GOARCH)
		}
		p.Method = MethodLinuxPackage
		p.PackageManager = pm.Name
		p.Asset = asset
		if err := downloadAndVerify(ctx, &p, rel, deps); err != nil {
			return Plan{}, err
		}
		return p, nil
	}

	// Fallback: tarball self-replace (macOS without brew, or Linux with no
	// known package manager present).
	asset, ok := matchAsset(rel.Assets, deps.GOOS, deps.GOARCH, ".tar.gz")
	if !ok {
		return Plan{}, fmt.Errorf("%w: no tarball for %s/%s", ErrNoMatchingAsset, deps.GOOS, deps.GOARCH)
	}
	p.Method = MethodTarballSelfReplace
	p.Asset = asset
	if err := downloadAndVerify(ctx, &p, rel, deps); err != nil {
		return Plan{}, err
	}
	return p, nil
}

// downloadAndVerify fetches p.Asset into a temp file and checks its SHA-256
// against the release's checksums.txt, setting p.LocalPath/ChecksumVerified.
func downloadAndVerify(ctx context.Context, p *Plan, rel Release, deps Deps) error {
	var sums Asset
	var ok bool
	for _, a := range rel.Assets {
		if a.Name == "checksums.txt" {
			sums, ok = a, true
			break
		}
	}
	if !ok {
		return fmt.Errorf("%w: release has no checksums.txt", ErrNoMatchingAsset)
	}
	sumsData, err := deps.Fetcher.FetchAll(ctx, sums.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("fetch checksums.txt: %w", err)
	}
	wantSum, ok := parseChecksums(sumsData, p.Asset.Name)
	if !ok {
		return fmt.Errorf("%w: %s not listed in checksums.txt", ErrChecksumMismatch, p.Asset.Name)
	}

	path, err := deps.Fetcher.FetchToFile(ctx, p.Asset.BrowserDownloadURL, deps.TempDir, "omac-update-*-"+p.Asset.Name)
	if err != nil {
		return fmt.Errorf("download %s: %w", p.Asset.Name, err)
	}
	defer func() {
		if p.LocalPath == "" {
			os.Remove(path)
		}
	}()
	gotSum, err := sha256File(path)
	if err != nil {
		return fmt.Errorf("checksum %s: %w", p.Asset.Name, err)
	}
	if !strings.EqualFold(gotSum, wantSum) {
		return fmt.Errorf("%w: %s", ErrChecksumMismatch, p.Asset.Name)
	}
	p.LocalPath = path
	p.ChecksumVerified = true
	return nil
}

// parseChecksums looks up name in the standard `sha256sum`-style
// "<hash>  <filename>" lines produced by checksums.txt.
func parseChecksums(data []byte, name string) (sum string, ok bool) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[1] == name || filepath.Base(fields[1]) == name {
			return fields[0], true
		}
	}
	return "", false
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Apply installs the update described by plan.
func Apply(ctx context.Context, plan Plan, deps Deps) error {
	switch plan.Method {
	case MethodUpToDate:
		return nil
	case MethodBrew:
		return deps.Runner.Run(ctx, "brew", []string{"upgrade", "oh-my-agentic-coder"}, deps.Stdin, deps.Stdout, deps.Stderr)
	case MethodLinuxPackage:
		var pm PackageManager
		for _, c := range deps.PkgManagers {
			if c.Name == plan.PackageManager {
				pm = c
				break
			}
		}
		if pm.Name == "" {
			return fmt.Errorf("unknown package manager %q", plan.PackageManager)
		}
		args := append([]string{pm.Name}, pm.InstallArgs(plan.LocalPath)...)
		return deps.Runner.Run(ctx, "sudo", args, deps.Stdin, deps.Stdout, deps.Stderr)
	case MethodTarballSelfReplace:
		return applySelfReplace(plan, deps)
	default:
		return fmt.Errorf("unknown update method %d", plan.Method)
	}
}

func applySelfReplace(plan Plan, deps Deps) error {
	f, err := os.Open(plan.LocalPath)
	if err != nil {
		return err
	}
	defer f.Close()

	data, mode, err := ExtractBinary(f, "omac")
	if err != nil {
		return fmt.Errorf("extract omac from %s: %w", plan.Asset.Name, err)
	}

	target, err := deps.Executable()
	if err != nil {
		return fmt.Errorf("resolve running binary path: %w", err)
	}
	if real, err := filepath.EvalSymlinks(target); err == nil {
		target = real
	}

	return deps.Replacer.Replace(target, bytes.NewReader(data), mode)
}
