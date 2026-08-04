package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// buildApproveSub is the literal subcommand dispatched to runBuildApprove.
const buildApproveSub = "approve"

// runBuildApprove implements `omac build approve [--root <rel>]`: a
// host-only transition that renders the consolidated capability diff
// for the worktree's build manifest, stores a durable approval ONLY
// after explicit interactive confirmation, and NEVER executes build
// code (ticket 06).
//
// Hardening rules (spec §Authorization and security, ticket 06):
//
//   - Refused in any managed session: when OMAC_BUILD_BROKER_REQUIRED=1
//     or any partial OMAC session env (OMAC_SOCKET/OMAC_BASE/
//     OMAC_CONTROL_BASE/OMAC_BUILD_TOKEN) is present, approve exits
//     with a policy denial (exit 3) and a "run from an interactive host
//     terminal" diagnostic. An agent cannot approve its own capability
//     set.
//   - Requires an interactive host terminal: stdin must be a TTY
//     (isInteractive returns true). Non-interactive invocation (piped
//     stdin, CI runner without a TTY) is refused with the same
//     diagnostic so an agent or script cannot auto-confirm.
//   - Renders the consolidated capability diff BEFORE writing any
//     durable record: the host user sees exactly what they are
//     approving. A missing manifest (the normal standard-Gradle-project
//     case) is a no-op success with a "no manifest to approve" message.
//   - Stores a durable approval only after explicit confirmation: the
//     user must type one of y/yes/confirm; anything else aborts without
//     writing.
//   - Never executes build code: approve reads the manifest, computes
//     the digest + capability set, renders the diff, and (on confirm)
//     writes the durable approval record under the host-only
//     build-control root. It does NOT start proxies, derive grants,
//     acquire the leaf lock, or launch the executor.
//   - A changed approval takes effect in start/serve only after parent
//     restart: the parent freezes the in-memory capability snapshot at
//     activation (or before launch); ordinary agent-callable
//     activate/reload routes can never grant or refresh build
//     capabilities.
func runBuildApprove(args []string, env *Env) int {
	// 1. Refused in any managed session. An agent cannot approve its
	//    own capability set.
	mode, _ := decideManagedMode()
	if mode != managedModeDirect {
		fmt.Fprintln(env.Stderr, "omac build approve: refused in a managed session — run from an interactive host terminal after the omac parent has stopped.")
		return ExitBuildPolicyDenied
	}
	// Also refuse when any partial OMAC session env is present even
	// without the required marker (defense in depth: decideManagedMode
	// already maps partial → failClosed, but approve is a hardening
	// gate so it checks the same condition explicitly).
	if os.Getenv(envBuildBrokerRequired) == "1" || os.Getenv(envControlBase) != "" ||
		os.Getenv(envBuildToken) != "" || os.Getenv(envOmacSocket) != "" ||
		os.Getenv(envOmacBase) != "" {
		fmt.Fprintln(env.Stderr, "omac build approve: refused: OMAC session environment detected — run from a clean interactive host terminal after the omac parent has stopped.")
		return ExitBuildPolicyDenied
	}

	// 2. Requires an interactive host terminal.
	if !isInteractive(env.Stdin) {
		fmt.Fprintln(env.Stderr, "omac build approve: refused: stdin is not a TTY — run from an interactive host terminal and confirm the capability diff.")
		return ExitBuildPolicyDenied
	}

	// 3. Parse --root (mirrors `omac build stop`'s grammar).
	root, perr := parseApproveArgs(args)
	if perr != nil {
		fmt.Fprintf(env.Stderr, "omac build approve: %v\n", perr)
		return ExitBuildPolicyDenied
	}

	// 4. Resolve the cache scope the same way `omac build` does so the
	//    durable approval record lands at the host-only build-control
	//    root keyed by canonical worktree.
	cacheDir, closeScope, err := prepareBuildCache(env.Workdir, "")
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac build approve: resolve cache scope: %v\n", err)
		return buildrun.ExitServiceFailure
	}
	defer closeScope()

	canonWorktree, err := canonicalWorktree(env.Workdir)
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac build approve: canonicalize worktree: %v\n", err)
		return ExitBuildPolicyDenied
	}
	_ = root // root is validated but not used to locate the manifest —
	// the manifest is always at <worktree>/.omac/build.yaml (the
	// approved capability set is worktree-scoped, not root-scoped).

	// 5. Load the manifest. A missing manifest is the normal
	//    standard-Gradle-project case: nothing to approve.
	host := buildmanifest.HostPolicy{} // approve freezes the default ceiling
	manifest, err := buildmanifest.Load(canonWorktree)
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac build approve: %v\n", err)
		return ExitBuildPolicyDenied
	}
	if !manifest.HasManifest() {
		fmt.Fprintln(env.Stdout, "omac build approve: no .omac/build.yaml manifest in this worktree — nothing to approve.")
		return ExitOK
	}
	if err := manifest.Validate(host); err != nil {
		fmt.Fprintf(env.Stderr, "omac build approve: %v\n", err)
		return ExitBuildPolicyDenied
	}
	caps := manifest.CapabilitySet(host)
	digest := buildmanifest.Digest(manifest)

	// 6. Render the consolidated capability diff for review. Compare
	//    against the existing durable approval (if any) so the host
	//    sees what changed.
	loc := buildControlApprovalLocation(cacheDir, canonWorktree)
	leaf := buildrun.GradleLeaf(cacheDir)
	prev, _ := buildmanifest.LoadApprovalAt(leaf, loc)
	diff := buildmanifest.Diff(prev.Capabilities, caps)
	fmt.Fprintln(env.Stdout, "omac build approve: review the capability diff before approving.")
	fmt.Fprintln(env.Stdout, diff.Render())
	fmt.Fprintf(env.Stdout, "Manifest digest: %s\n", shortDigestHex(digest))
	fmt.Fprintln(env.Stdout, "\nApproving stores a durable approval record under the host-only build-control root.")
	fmt.Fprintln(env.Stdout, "The approval takes effect in `omac start`/`serve` only after the parent restarts.")
	fmt.Fprintln(env.Stdout, "An agent-callable activate/reload route can never grant or refresh build capabilities.")
	fmt.Fprintln(env.Stdout, "\nType 'yes' (or 'confirm') to approve, anything else to abort:")

	// 7. Explicit confirmation from the interactive host terminal.
	reader := bufio.NewReader(env.Stdin)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	if line != "y" && line != "yes" && line != "confirm" {
		fmt.Fprintln(env.Stdout, "omac build approve: aborted — no approval written.")
		return ExitOK
	}

	// 8. Store the durable approval. This is the ONLY write; it does
	//    NOT execute build code, start proxies, or launch the executor.
	//    In addition to the per-worktree record, when the worktree's
	//    repo identity is resolvable (a git repo with a root commit),
	//    it writes the digest-indexed, repo-namespaced reuse record
	//    (with repoRootCommit) under the host-only approvals-by-repo
	//    tree (ADR 0005). This record is INERT unless the host enables
	//    approval reuse; writing it at approve time means an
	//    already-approved repo's unchanged worktrees can reuse the
	//    approval later without re-running approve.
	if err := buildmanifest.ApproveAt(leaf, loc, digest, caps); err != nil {
		fmt.Fprintf(env.Stderr, "omac build approve: write approval: %v\n", err)
		return buildrun.ExitServiceFailure
	}
	// Digest-indexed reuse record (idempotent; best-effort — a
	// non-resolvable repo identity merely skips it, it is not an
	// approval error).
	cacheRoot := buildControlCacheRoot(cacheDir)
	if cacheRoot != "" {
		if canonRepoRoot, rootCommit := resolveRepoIdentity(canonWorktree); canonRepoRoot != "" && rootCommit != "" {
			reuseRec := buildmanifest.ApprovalRecord{
				Digest:         digest,
				Capabilities:   caps,
				ApprovedAt:     time.Now().UTC(),
				RepoRootCommit: rootCommit,
			}
			if err := buildmanifest.StoreApprovalForRepoDigest(cacheRoot, canonRepoRoot, digest, reuseRec); err != nil {
				// A reuse-record write failure must not fail the already
				// successful per-worktree approval (the reuse record is a
				// convenience, never an approval transition).
				fmt.Fprintf(env.Stderr, "omac build approve: warning: digest-indexed reuse record not written: %v\n", err)
			}
		}
	}
	fmt.Fprintln(env.Stdout, "omac build approve: durable approval recorded. Restart the omac parent (`omac start`/`serve`) to activate the capability set.")
	return ExitOK
}

// parseApproveArgs parses the args for `omac build approve [--root <rel>]`
// (and `--root=<rel>`), mirroring `omac build stop`'s grammar. Any
// other flag is a policy denial. Returns the resolved root ("." when
// no --root is supplied) or an error.
func parseApproveArgs(args []string) (string, error) {
	root := "."
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--root":
			if i+1 >= len(args) {
				return "", errors.New("--root requires a value")
			}
			root = args[i+1]
			i++
		case strings.HasPrefix(a, "--root="):
			root = strings.TrimPrefix(a, "--root=")
		case a == "--":
			i = len(args)
		default:
			return "", fmt.Errorf("unknown flag %q (usage: omac build approve [--root <rel>])", a)
		}
	}
	if root == "" {
		return "", errors.New("--root must not be empty")
	}
	return root, nil
}

// isInteractive reports whether f is a terminal (a TTY). Approve
// requires an interactive host terminal so an agent or script with
// piped stdin cannot auto-confirm. f must be non-nil; a nil f returns
// false.
func isInteractive(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	// A terminal device file has ModeCharDevice set; a pipe or regular
	// file does not. This is the standard Go TTY check.
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// shortDigestHex returns the first 8 chars of a digest for diagnostics.
func shortDigestHex(d string) string {
	if len(d) > 8 {
		return d[:8]
	}
	return d
}
