package updater

import (
	"context"
	"os/exec"
	"strings"
)

// SelfCheckResult reports whether the omac a shell will actually invoke after
// an update is the freshly-installed version, or an older binary earlier on
// PATH shadowing it.
type SelfCheckResult struct {
	// ResolvedPath is where `omac` resolves on PATH after the install.
	ResolvedPath string
	// ResolvedVersion is that binary's self-reported version.
	ResolvedVersion string
	// Shadowed is true when ResolvedVersion is not the version just installed,
	// i.e. a stale binary earlier on PATH is shadowing the update.
	Shadowed bool
}

// SelfCheck runs after a successful install to confirm the `omac` a user's
// shell will pick up is the version that was just installed. Package-manager
// and brew installs write to a fixed, package-owned path; if an older binary
// (e.g. a prior tarball self-replace in ~/.local/bin) sits earlier on PATH,
// it silently shadows the update and every subsequent `omac update` re-runs
// against the stale version. This detects exactly that so the caller can
// suggest removing the shadow.
//
// A probe failure (omac not found on PATH, version subcommand errors) returns
// an error; the caller treats it as a soft, non-fatal note since the install
// itself already succeeded.
func SelfCheck(ctx context.Context, plan Plan, deps Deps) (SelfCheckResult, error) {
	path, err := deps.PathLookup("omac")
	if err != nil {
		return SelfCheckResult{}, err
	}
	version, err := deps.VersionProbe(ctx, path)
	if err != nil {
		return SelfCheckResult{ResolvedPath: path}, err
	}
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	return SelfCheckResult{
		ResolvedPath:    path,
		ResolvedVersion: version,
		Shadowed:        version != plan.LatestVersion,
	}, nil
}

// probeVersion runs `<path> version` and extracts the reported version token
// from its "omac <version>" output.
func probeVersion(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return "", err
	}
	return parseVersionOutput(string(out)), nil
}

// parseVersionOutput pulls the version token out of `omac version` output,
// which is a single "omac <version>" line.
func parseVersionOutput(out string) string {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}
