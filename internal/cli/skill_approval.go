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
// funnels through it even if a caller forgets its own pre-flight check.
func skillSpawnAuthorizer(spec supervisor.SidecarSpec) error {
	name := spec.SkillName
	if name == "" {
		name = spec.Name
	}
	return approvalRefusal(name, spec.SkillDir, "")
}

// approvalRefusal reports whether skill `name` may spawn from the content
// currently on disk at skillDir. A nil error means approved.
//
// bundleHash may be a hash the caller already computed for skillDir (the
// launch paths run a drift check first); pass "" to hash it here.
//
// A non-nil error is the refusal reason, suitable verbatim as a broken-route
// Detail — errSkillNotApproved when there is simply no approval, or the
// underlying cause (unreadable skill dir, corrupt approval store) so the
// operator is not told to re-register when that cannot help. Any error fails
// closed: the skill does not spawn.
func approvalRefusal(name, skillDir, bundleHash string) error {
	if bundleHash == "" {
		h, err := config.BundleHash(skillDir)
		if err != nil {
			return fmt.Errorf("skill %q: cannot hash %s: %w", name, skillDir, err)
		}
		bundleHash = h
	}
	approved, err := skilltrust.IsApproved(name, bundleHash)
	if err != nil {
		return fmt.Errorf("skill %q: cannot read the host approval store: %w", name, err)
	}
	if !approved {
		return errSkillNotApproved(name)
	}
	return nil
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
