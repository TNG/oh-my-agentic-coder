package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildcontrol"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// envApprovalReuseByDigest is the ad-hoc env override that enables the
// opt-in digest-bound approval-reuse-by-digest feature (ADR 0005).
// Host-only: a managed (sandboxed agent) session cannot set it and
// cannot write approval records, so the agent cannot enable reuse.
const envApprovalReuseByDigest = "OMAC_APPROVAL_REUSE_BY_DIGEST"

// approvalReuseEnabled reports whether the opt-in approval-reuse-by-
// digest feature (ADR 0005) is enabled, via the env override AND/OR the
// host config entry in `<cacheRoot>/build-control/config.toml`. Both act
// as a durable default (config) with an ad-hoc override (env). Default
// off. cacheRoot is the shared cache root (parent of cache-scope dirs);
// an empty cacheRoot disables reuse.
func approvalReuseEnabled(cacheRoot string) bool {
	if os.Getenv(envApprovalReuseByDigest) == "1" {
		return true
	}
	return approvalReuseFromHostConfig(cacheRoot)
}

// approvalReuseFromHostConfig reads the host control configuration at
// `<cacheRoot>/build-control/config.toml` and reports whether it enables
// approval reuse. A missing/unreadable file, or the absence of the key,
// means reuse is off. The scan is deliberately minimal (no TOML
// dependency): it looks for a top-level `approval_reuse_by_digest = <bool>`
// line. A malformed value is treated as off (fail closed).
func approvalReuseFromHostConfig(cacheRoot string) bool {
	if cacheRoot == "" {
		return false
	}
	path := filepath.Join(buildcontrol.Root(cacheRoot), "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) != "approval_reuse_by_digest" {
			continue
		}
		v := strings.TrimSpace(strings.Trim(strings.TrimSpace(value), `"`))
		b, err := strconv.ParseBool(v)
		if err != nil {
			return false
		}
		return b
	}
	return false
}

// resolveRepoIdentity resolves the two-part repo identity used by the
// opt-in approval-reuse-by-digest feature (ADR 0005):
//
//   - canonicalRepoRoot — the canonical (EvalSymlinks-resolved)
//     `git -C <worktree> rev-parse --git-common-dir` output. All linked
//     worktrees of a repo share the same common dir, so they collapse to
//     the same namespace.
//   - repoRootCommit — the SHA of the first commit in the repo's
//     history (`git -C <worktree> rev-list --max-parents=0 HEAD`), the
//     recycling guard stored in the digest-indexed reuse record.
//
// Non-git / empty-repo / missing-common-dir (or any git error) returns
// BOTH empty and nil error: identity is not ermittelbar, so the caller
// falls back to per-worktree approval with NO error (spec §Edge cases).
func resolveRepoIdentity(worktree string) (canonicalRepoRoot, repoRootCommit string) {
	common, err := gitCommonDir(worktree, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(worktree, common)
	}
	canon, err := filepath.EvalSymlinks(common)
	if err != nil {
		return "", ""
	}
	rootCommit, err := gitCommonDir(worktree, "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		return "", ""
	}
	return canon, rootCommit
}

// gitCommonDir runs git in `-C <dir>` fashion (c.Dir = worktree) with a
// hermetic-enough environment (no host git config interference beyond
// what the shell inherits) and returns trimmed stdout. Used to resolve
// the repo identity for approval reuse.
func gitCommonDir(worktree string, args ...string) (string, error) {
	c := exec.Command("git", args...)
	c.Dir = worktree
	out, err := c.CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// newDirectSnapshotProvider returns a SnapshotProvider for the direct
// host-terminal build path that, in addition to the existing
// behavior-preserving per-worktree gate, consults the opt-in
// digest-indexed approval-reuse fallback (ADR 0005) when reuse is
// enabled and the repo identity is resolvable. cacheRoot is the shared
// cache root (empty → reuse never consulted). The per-worktree approval
// location stays the legacy OnLeaf (the direct path's historical
// layout); the reuse record lives under the host-only build-control
// root's approvals-by-repo tree.
//
// The returned provider mirrors buildengine.DirectSnapshotProvider
// exactly (Load → Validate → CapabilitySet → Digest → Gate) except that
// the gate receives the reuse context.
func newDirectSnapshotProvider(cacheRoot string) buildengine.SnapshotProvider {
	reuse := cacheRoot != "" && approvalReuseEnabled(cacheRoot)
	return func(worktree, leaf string, req buildrun.Request) (buildengine.PolicySnapshot, error) {
		hostPolicy := buildrun.HostPolicy(req.MaxDuration)
		manifest, err := buildmanifest.Load(worktree)
		if err != nil {
			return buildengine.PolicySnapshot{}, err
		}
		if err := manifest.Validate(hostPolicy); err != nil {
			return buildengine.PolicySnapshot{}, err
		}
		if !manifest.HasManifest() {
			return buildengine.PolicySnapshot{HostPolicy: hostPolicy}, nil
		}
		caps := manifest.CapabilitySet(hostPolicy)
		digest := buildmanifest.Digest(manifest)
		var opts buildmanifest.GateOptions
		if reuse {
			canonRepoRoot, rootCommit := resolveRepoIdentity(worktree)
			if canonRepoRoot != "" && rootCommit != "" {
				opts = buildmanifest.GateOptions{
					EnableReuse:        true,
					CanonicalRepoRoot:  canonRepoRoot,
					RepoRootCommit:     rootCommit,
					RepoDigestLocation: buildmanifest.NewRepoDigestLocation(cacheRoot, canonRepoRoot),
				}
			}
		}
		gateRes, gerr := buildmanifest.GateAt(leaf, buildmanifest.NewOnLeafLocation(), digest, caps, opts)
		if gerr != nil {
			return buildengine.PolicySnapshot{}, gerr
		}
		return buildengine.PolicySnapshot{
			Digest:       gateRes.Digest,
			Capabilities: gateRes.Capabilities,
			HostPolicy:   hostPolicy,
		}, nil
	}
}
