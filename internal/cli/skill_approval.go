package cli

import (
	"fmt"
	"path/filepath"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/skilltrust"
	"github.com/tngtech/oh-my-agentic-coder/internal/supervisor"
)

// A skill sidecar runs UNSANDBOXED, so it may spawn only when its current
// on-disk content is recorded in the host-only approval store the sandbox
// cannot write. See internal/skilltrust and docs/SECURITY_MODEL.md.

// skillSpawnAuthorizer is the supervisor spawn-gate backstop: every spawn
// funnels through it even if a caller forgets its own pre-flight check. A
// sidecar may run only from the host-only approval snapshot, which the sandbox
// cannot write; anything else is refused.
//
// Checking the LOCATION is stronger than re-hashing the dir, which is why it is
// the backstop: a hash check accepts ANY path whose content matches an approval,
// including the agent-writable workdir itself, and re-opens the check-then-exec
// TOCTOU that snapshotting exists to close. The pre-flight approvedSpawnDir is
// what maps an approved skill to its snapshot dir.
func skillSpawnAuthorizer(spec supervisor.SidecarSpec) error {
	if skilltrust.IsSnapshotPath(spec.SkillDir) {
		return nil
	}
	name := spec.SkillName
	if name == "" {
		name = spec.Name
	}
	// Refuse outright — deliberately NOT falling back to a content re-hash of
	// spec.SkillDir. Re-hashing would accept any path whose bytes match an
	// approval, including the agent-writable workdir, which is exactly the
	// weaker check this backstop exists to avoid.
	return errSkillNotApproved(name)
}

// skillBundleHash returns bundleHash when the caller already computed one for
// skillDir, otherwise hashes the directory. Keeping it in one place means a
// skill tree is walked at most once per launch path.
func skillBundleHash(name, skillDir, bundleHash string) (string, error) {
	if bundleHash != "" {
		return bundleHash, nil
	}
	h, err := config.BundleHash(skillDir)
	if err != nil {
		return "", fmt.Errorf("skill %q: cannot hash %s: %w", name, skillDir, err)
	}
	return h, nil
}

// approvalRefusal reports whether skill `name` may spawn given the content
// currently on disk at skillDir. A nil error means approved.
//
// A non-nil error is the refusal reason, suitable verbatim as a broken-route
// Detail — errSkillNotApproved when there is simply no approval, or the
// underlying cause (unreadable skill dir, corrupt approval store) so the
// operator is not told to re-register when that cannot help. Any error fails
// closed: the skill does not spawn.
func approvalRefusal(name, skillDir, bundleHash string) error {
	h, err := skillBundleHash(name, skillDir, bundleHash)
	if err != nil {
		return err
	}
	approved, err := skilltrust.IsApproved(name, h)
	if err != nil {
		return fmt.Errorf("skill %q: cannot read the host approval store: %w", name, err)
	}
	if !approved {
		return errSkillNotApproved(name)
	}
	return nil
}

// approvedSpawnDir resolves the directory a skill's sidecar must be spawned
// FROM: the immutable, host-only snapshot frozen at approval time (see
// internal/skilltrust snapshot.go). workdirSkillDir is the agent-writable
// source; its content is matched against the approval, then the corresponding
// snapshot is returned. Spawning from the snapshot, not the workdir, is what
// makes the executed bytes exactly the approved bytes.
//
// bundleHash may be a hash the caller already computed for workdirSkillDir;
// pass "" to hash it here. A nil error means approved; a non-nil error is the
// refusal detail (see approvalRefusal).
func approvedSpawnDir(name, workdirSkillDir, bundleHash string) (snapshotDir string, refusal error) {
	h, err := skillBundleHash(name, workdirSkillDir, bundleHash)
	if err != nil {
		return "", err
	}
	if refusal := approvalRefusal(name, workdirSkillDir, h); refusal != nil {
		return "", refusal
	}
	// An approval whose snapshot is missing (a hand-deleted ~/.config/omac
	// tree) is recoverable by re-registering, so refuse this one skill here
	// rather than letting the supervisor backstop fail the whole launch.
	snap, present := skilltrust.SnapshotPath(name, h)
	if !present {
		return "", errSkillNotApproved(name)
	}
	return snap, nil
}

// errSkillNotApproved is the uniform refusal error/detail; it names the
// host-side remedy the sandboxed agent cannot perform itself.
func errSkillNotApproved(name string) error {
	return fmt.Errorf("spawn refused: skill %q is not host-approved; run `omac register %s` from a host terminal (outside the sandbox) to approve its current code", name, name)
}

// refusalNotice is the operator-facing line the launch paths print when a
// skill is left unavailable, kept in one place so start and reload agree. The
// refusal already names the skill and the remedy, so this only adds what
// happened to the route.
func refusalNotice(refusal error) string {
	return fmt.Sprintf("%v (mounted as unavailable)", refusal)
}

// brokenApprovalRoute is the facade route an unapproved skill is mounted as:
// a broken (502) stub naming the remedy, so a probe is answered instead of
// 404'd — and no sidecar is spawned.
func brokenApprovalRoute(mount, name, skillDir string, refusal error) facade.Route {
	return facade.Route{
		Mount:    mount,
		Skill:    name,
		SkillDir: skillDir,
		State:    facade.RouteBroken,
		Detail:   refusal.Error(),
	}
}

// grandfatherScope is one registry layer to grandfather, plus the workdir its
// relative SkillDirs resolve against. workdir is "" for the user-global layer,
// whose entries must already be absolute.
type grandfatherScope struct {
	workdir string
	reg     *registry.Registry
}

// grandfatherOnce approves everything currently registered in scopes and then
// persists the approval store, so the trust-on-first-upgrade window closes even
// when nothing was approved (without that, a host with no registered skills
// would look like a first upgrade on every launch and keep re-opening it).
//
// Callers must guard it with firstApprovalUpgrade and own the human-facing
// message; keeping the approve-then-close pair here means no entry point can
// implement half of it. Returns the number of skills approved.
func grandfatherOnce(scopes ...grandfatherScope) (int, error) {
	n := 0
	var firstErr error
	for _, s := range scopes {
		if s.reg == nil {
			continue
		}
		c, err := grandfatherApprovals(s.workdir, s.reg)
		n += c
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Close the window regardless: a partial migration must not be retried
	// on the next launch, when an agent-authored skill could be registered.
	if err := skilltrust.EnsureInitialized(); err != nil && firstErr == nil {
		firstErr = err
	}
	return n, firstErr
}

// grandfatherApprovals approves every skill in reg using the hash of its
// content ON DISK (what will actually run), resolving workdir-relative
// SkillDirs against workdir. Callers reach this only on the first upgraded
// run (see firstApprovalUpgrade): before this control existed every
// registered skill already spawned at launch, so upgrading omac must not
// newly break a working setup. This is trust-on-first-upgrade — it approves
// whatever is registered right now, once; everything registered afterwards
// needs an explicit out-of-sandbox `omac register`. Returns the count
// approved. A skill whose dir cannot be resolved or hashed is skipped (the
// spawn gate will refuse it and name the remedy).
func grandfatherApprovals(workdir string, reg *registry.Registry) (int, error) {
	n := 0
	for _, e := range reg.Registered {
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			if workdir == "" {
				continue // global entries must be absolute; nothing to resolve against
			}
			absDir = filepath.Join(workdir, absDir)
		}
		hash, herr := config.BundleHash(absDir)
		if herr != nil {
			continue
		}
		if err := skilltrust.Approve(e.Name, hash, absDir); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// firstApprovalUpgrade reports whether the host-only approval store has never
// been created — i.e. this is the first run under approval-gated spawning.
// Capture it once at an entry point's start, BEFORE any grandfathering (which
// creates the store), so every registry loaded during that first run is
// grandfathered while later runs are not.
func firstApprovalUpgrade() bool { return !skilltrust.Exists() }
