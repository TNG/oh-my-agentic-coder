package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skillstate"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
	"github.com/TNG/oh-my-agentic-coder/internal/supervisor"
)

// A skill sidecar runs UNSANDBOXED, so it may spawn only when its current
// on-disk content is recorded in the host-only approval store the sandbox
// cannot write. See internal/skilltrust and docs/security.md.

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

// skillCopy names which registered copy of a skill a refusal is about. The
// same name can be registered once per harness and both workdir-local and
// user-global, and `omac register <name>` refuses such a name as ambiguous, so
// the remedy must carry the flags that select this copy (issue #514).
type skillCopy struct {
	Name string
	// Harness is the registry entry's harness scope ("opencode",
	// "claude-code"); "" for a legacy entry or a skill under the shared
	// .agents base, which every harness sees.
	Harness string
	// Dir is the absolute skill directory the content was read from.
	Dir string
	// Global marks a user-global registration (absolute SkillDir outside the
	// workdir), which `omac register` selects with --global.
	Global bool
}

// skillCopyOf derives the copy a registry entry denotes. absDir is the entry's
// SkillDir resolved against workdir; workdir may be "" for a user-global
// layer, whose entries are absolute by construction.
func skillCopyOf(e registry.Entry, absDir, workdir string) skillCopy {
	global := filepath.IsAbs(e.SkillDir) && (workdir == "" || !isUnderDir(workdir, absDir))
	return skillCopy{Name: e.Name, Harness: e.Harness, Dir: absDir, Global: global}
}

// registerCmd is the `omac register` invocation that resolves to exactly this
// copy, in the flag order the command documents (see hints.go registerCmd).
func (c skillCopy) registerCmd() string {
	var flags []string
	if c.Harness != "" {
		flags = append(flags, "--harness", c.Harness)
	}
	if c.Global {
		flags = append(flags, "--global")
	}
	return registerCmd(c.Name, flags...)
}

// String renders the copy for a refusal line: the quoted name, then the
// harness, scope and directory when they are known, so a user with the same
// name registered twice can see which one is meant.
func (c skillCopy) String() string {
	q := fmt.Sprintf("%q", c.Name)
	var where []string
	if c.Harness != "" {
		where = append(where, c.Harness)
	}
	if c.Global {
		where = append(where, "global")
	}
	if c.Dir != "" {
		where = append(where, c.Dir)
	}
	if len(where) == 0 {
		return q
	}
	return q + " (" + strings.Join(where, ", ") + ")"
}

// spawnRefusal is a spawn-gate refusal that re-registering from a host
// terminal resolves. Error() is the agent-facing detail (broken-route Detail,
// reload log line); Problem() is what the human-facing launch report and
// `omac doctor` group by.
type spawnRefusal struct {
	Kind  skillstate.ProblemKind // NotApproved, SnapshotStale or SnapshotMissing
	Skill skillCopy
}

func (r *spawnRefusal) Error() string {
	cmd := "run `" + r.Skill.registerCmd() + "` from a host terminal (outside the sandbox)"
	switch r.Kind {
	case skillstate.SnapshotStale:
		// Names both the policy (so a future bump can reuse the text) and
		// the release that bumped it (so the user can tie it to the upgrade).
		return fmt.Sprintf("spawn refused: skill %s is approved, but its snapshot was made by an older copy policy (registered before %s) and must be rebuilt once; %s to rebuild it",
			r.Skill, skilltrust.SnapshotPolicySince, cmd)
	case skillstate.SnapshotMissing:
		return fmt.Sprintf("spawn refused: skill %s is approved, but its snapshot is missing from the host store; %s to rebuild it", r.Skill, cmd)
	default:
		return fmt.Sprintf("spawn refused: skill %s is not host-approved; %s to approve its current code", r.Skill, cmd)
	}
}

// Problem is the refusal as a skillstate.Problem. The cause is not repeated
// here: every renderer takes it from spawnRefusalClasses by Kind, so Detail
// stays empty and Fix carries the command.
func (r *spawnRefusal) Problem() skillstate.Problem {
	return skillstate.Problem{Kind: r.Kind, Skill: r.Skill.Name, Fix: r.Skill.registerCmd(), Cause: r}
}

// refusalProblem turns any spawn-gate error for skill name into a Problem: a
// spawnRefusal keeps its class and remedy; anything else (unreadable dir,
// corrupt store) becomes SpawnRefused with the cause verbatim and no Fix, so
// the report never tells the user to re-register when that cannot help.
func refusalProblem(name string, err error) skillstate.Problem {
	var r *spawnRefusal
	if errors.As(err, &r) {
		return r.Problem()
	}
	return skillstate.Problem{Kind: skillstate.SpawnRefused, Skill: name, Detail: err.Error(), Cause: err}
}

// approvalRefusal reports whether the registered copy c may spawn given the
// content currently on disk at c.Dir. A nil error means approved.
//
// A non-nil error is the refusal reason, suitable verbatim as a broken-route
// Detail: a *spawnRefusal naming the command that selects this copy when there
// is simply no approval, or the underlying cause (unreadable skill dir, corrupt
// approval store) so the operator is not told to re-register when that cannot
// help. Any error fails closed: the skill does not spawn.
func approvalRefusal(c skillCopy, bundleHash string) error {
	h, err := skillBundleHash(c.Name, c.Dir, bundleHash)
	if err != nil {
		return err
	}
	approved, err := skilltrust.IsApproved(c.Name, h)
	if err != nil {
		return fmt.Errorf("skill %q: cannot read the host approval store: %w", c.Name, err)
	}
	if !approved {
		return &spawnRefusal{Kind: skillstate.NotApproved, Skill: c}
	}
	return nil
}

// approvedSpawnDir resolves the directory a skill's sidecar must be spawned
// FROM: the immutable, host-only snapshot frozen at approval time (see
// internal/skilltrust snapshot.go). c.Dir is the agent-writable source; its
// content is matched against the approval, then the corresponding snapshot is
// returned. Spawning from the snapshot, not the workdir, is what makes the
// executed bytes exactly the approved bytes.
//
// bundleHash may be a hash the caller already computed for c.Dir; pass "" to
// hash it here. A nil error means approved. A refusal that re-registering
// fixes is a *spawnRefusal whose Kind says which case applies: no approval, a
// snapshot from before skilltrust.SnapshotPolicySince, or no snapshot at all.
func approvedSpawnDir(c skillCopy, bundleHash string) (snapshotDir string, refusal error) {
	if err := config.ValidSkillName(c.Name); err != nil {
		return "", fmt.Errorf("spawn refused: %w", err)
	}
	h, err := skillBundleHash(c.Name, c.Dir, bundleHash)
	if err != nil {
		return "", err
	}
	if refusal := approvalRefusal(c, h); refusal != nil {
		return "", refusal
	}
	// An approval whose snapshot is unusable is recoverable by re-registering,
	// so refuse this one skill here rather than letting the supervisor
	// backstop fail the whole launch. Nothing deletes the retired snapshot
	// root on upgrade, so a legacy copy still being there is what tells the
	// upgrade case apart from a hand-deleted store.
	snap, present := skilltrust.SnapshotPath(c.Name, h)
	if !present {
		if skilltrust.LegacySnapshotPresent(c.Name, h) {
			return "", &spawnRefusal{Kind: skillstate.SnapshotStale, Skill: c}
		}
		return "", &spawnRefusal{Kind: skillstate.SnapshotMissing, Skill: c}
	}
	return snap, nil
}

// snapshotMeta loads the skill manifest from the approved snapshot directory.
// All spawn sites must derive Command, EnvPassthrough and Health from this
// rather than from the earlier workdir read, so the executed argv is always
// the approved one. A missing or broken manifest in the snapshot is a fatal
// refusal rather than a fallback to the workdir.
func snapshotMeta(name, snapDir string) (*config.Meta, error) {
	m, err := config.LoadMeta(filepath.Join(snapDir, config.MetaFileName))
	if err != nil {
		return nil, fmt.Errorf("spawn refused: skill %q snapshot manifest unreadable: %w", name, err)
	}
	if m.Sidecar == nil {
		return nil, fmt.Errorf("spawn refused: skill %q snapshot manifest has no sidecar block", name)
	}
	return m, nil
}

// errSkillNotApproved is the refusal for the supervisor backstop, which only
// knows a spec name: the bare remedy, since no registry entry is at hand.
func errSkillNotApproved(name string) error {
	return &spawnRefusal{Kind: skillstate.NotApproved, Skill: skillCopy{Name: name}}
}

// refusalNotice is the operator-facing line live reload prints when a skill is
// left unavailable (the launch itself prints one grouped report instead, see
// renderApprovalRefusals). The refusal already names the skill and the remedy,
// so this only adds what happened to the route.
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

// grandfatherApprovals approves user-global skills in reg on the first
// upgraded run (see firstApprovalUpgrade). Only entries whose SkillDir is
// absolute AND outside the workdir are approved: workdir-local entries are
// agent-writable and must not be auto-approved. The upgrade promise
// (docs/security.md) is scoped to skills the agent cannot forge.
//
// The registry entry's recorded BundleHash is the only evidence of the
// content a human reviewed at `omac register` time, so it anchors every
// approval: an entry is approved only when its recorded hash is non-empty
// and still matches the bytes on disk. Anything else — including legacy
// entries recorded without a hash — is skipped and left to the ordinary
// spawn gate, which refuses drifted skills and names the re-register
// remedy. Approving whatever is currently on disk would instead bless
// changes made after registration, which for user-global skills can
// include changes written inside the sandbox.
//
// A skill whose dir cannot be resolved or hashed is skipped (the spawn gate
// will refuse it and name the remedy). Returns the count approved.
func grandfatherApprovals(workdir string, reg *registry.Registry) (int, error) {
	n := 0
	for _, e := range reg.Registered {
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			// Relative SkillDir resolves inside the workdir — agent-writable,
			// not safe to grandfather.
			continue
		}
		// Even an absolute path may sit inside the workdir (e.g. the agent
		// computed and stored it as absolute). Skip those too.
		if workdir != "" && isUnderDir(workdir, absDir) {
			continue
		}
		// Legacy entries recorded without a hash carry no record of what
		// the human reviewed; skip them rather than trusting the disk.
		if e.BundleHash == "" {
			continue
		}
		hash, herr := config.BundleHash(absDir)
		if herr != nil || hash != e.BundleHash {
			continue
		}
		if err := skilltrust.Approve(e.Name, e.BundleHash, absDir); err != nil {
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

// isUnderDir reports whether path is inside or equal to root.
func isUnderDir(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	// rel is "." when equal, a plain relative path when inside, or starts
	// with ".." when outside.
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}
