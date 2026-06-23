## ADDED Requirements

### Requirement: omac ships the omac-write-a-skill bundle in the binary

omac SHALL embed the guidance-only `omac-write-a-skill` skill bundle (a `SKILL.md`,
no `omac.yaml`) in the binary so it is available without any network access or
external installer.

#### Scenario: Bundle present in a downloaded binary

- **WHEN** a user obtains omac via any supported distribution (brew, `.deb`, release
  tarball, `go install`) on a machine with no skills on disk
- **THEN** the `omac-write-a-skill` bundle is available from the binary itself, with no
  dependency on `opencode-nono` or any other installer

### Requirement: The shipped skill is omac-namespaced and collision-proof

The shipped skill SHALL use the `omac-` prefixed name `omac-write-a-skill` for both
its directory and its SKILL.md `name`, SHALL carry omac-ownership metadata (including
an `omac-builtin` marker), and its description SHALL be scoped to authoring omac
skills. It SHALL neither collide with nor be confused for third-party skill-authoring
skills (e.g. `writing-skills`, `skill-creator`).

#### Scenario: Coexists with a third-party authoring skill

- **WHEN** a third-party skill-authoring skill (e.g. `writing-skills`) is present in
  the same skills directory
- **THEN** the omac skill is surfaced alongside it with no name collision, because its
  name is `omac-write-a-skill`

#### Scenario: Provisioning never clobbers a foreign same-named skill

- **WHEN** `omac setup` runs and a directory of the same name exists that does NOT
  carry the omac-builtin marker
- **THEN** omac does not overwrite it and warns the user instead

#### Scenario: Selected for omac authoring specifically

- **WHEN** the agent is asked to author an omac skill
- **THEN** the omac-scoped description leads the harness to select `omac-write-a-skill`
  rather than a generic skill-writing skill

### Requirement: omac auto-provisions the skill on launch

`omac start` and `omac serve` SHALL idempotently write the embedded bundle into the
native user-global skills directory that the **active** harness's own loader reads
(e.g. `~/.config/opencode/skills` for OpenCode honoring `$XDG_CONFIG_HOME`,
`~/.claude/skills` for Claude Code) — with no separate setup step. Provisioning SHALL
NOT block or fail the launch, SHALL be silent when the bundle is already current, and
SHALL NOT rely on omac registration or activation (omac ignores `SKILL.md`-only
directories; the harness surfaces the skill itself).

#### Scenario: Available after a plain launch

- **WHEN** a user runs `omac start` (or `omac serve`) on a machine where the bundle is
  not yet on disk
- **THEN** the bundle directory (containing at least its `SKILL.md`) is written into
  the active harness's native skills directory, and that harness surfaces
  `omac-write-a-skill` — with no `omac register`, `omac setup`, or activation step

#### Scenario: Launch is not blocked by provisioning

- **WHEN** the bundle is already current, or its target cannot be written
- **THEN** launch proceeds normally — an already-current bundle produces no output,
  and a write failure is reported as a warning rather than aborting the launch

### Requirement: `omac setup` provisions all installed harnesses explicitly

omac SHALL provide an `omac setup` command that writes the embedded bundle into the
native skills directory of **each installed harness** (optionally narrowed to one via a
positional argument), for explicit or forced (re)provisioning after an upgrade.

#### Scenario: Provisioned to all installed harnesses

- **WHEN** a user runs `omac setup` with more than one harness installed
- **THEN** the bundle directory is written into each installed harness's native skills
  directory

### Requirement: Provisioning is idempotent

Re-running `omac setup` SHALL be safe: it overwrites only omac-owned bundle files
(those under a directory carrying the omac-builtin marker) to their embedded version,
leaves an already-current bundle unchanged, and reports what changed.

#### Scenario: Re-run after an omac upgrade

- **WHEN** the user runs `omac setup` again after upgrading omac
- **THEN** the on-disk bundle is refreshed to the version embedded in the new binary;
  re-running with no version change reports the bundle as unchanged

### Requirement: omac-write-a-skill carries the authoring guide

The `omac-write-a-skill` bundle SHALL make the skill-authoring guidance (the content
of `CREATING_A_SKILL.md`) available to the agent, and the bundle content SHALL be kept
byte-identical to the repository's authoring guide.

#### Scenario: Authoring guidance available in any project

- **WHEN** the agent needs to author a new skill in an arbitrary working directory
- **THEN** the `omac-write-a-skill` skill provides the authoring guidance without the
  user copying `CREATING_A_SKILL.md` into that project

### Requirement: Fresh-install availability needs no extra step

Following the README Quickstart on a clean machine SHALL yield a working
`omac-write-a-skill` skill without any provisioning step beyond launching omac.

#### Scenario: Following the Quickstart on a clean machine

- **WHEN** a new user installs omac and runs `omac start` (no `omac setup` step)
- **THEN** the `omac-write-a-skill` skill is present in the active harness's skills
  directory and surfaced by it, with no reference to the retired `opencode-nono`
  installer
