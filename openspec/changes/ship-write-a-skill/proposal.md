## Why

A skill-authoring helper should be available in every omac installation, but today it
only exists as the `CREATING_A_SKILL.md` guide in the repo root — there is no shipped
skill and nothing puts it on disk. The harnesses surface guidance skills from their
own skills directories, but nothing populates those on a fresh machine; that was
`opencode-nono`'s `install.sh`, which is no longer used. So the authoring helper is
not available in any project. omac needs to ship and provision this skill itself.

## What Changes

- omac **embeds an `omac-write-a-skill` skill bundle** in the binary (like the
  existing `omac-multidir.ts` bridge), derived from `CREATING_A_SKILL.md`. It is a
  **guidance-only** skill (a `SKILL.md`, no `omac.yaml` sidecar).
- The skill is **omac-namespaced** (`omac-` prefixed name + omac-ownership metadata +
  an omac-scoped description) so it never collides with, nor is confused for, popular
  third-party authoring skills such as `writing-skills` or `skill-creator`.
- **`omac start` / `omac serve` auto-provision** the embedded skill into the active
  harness's native skills directory (e.g. `~/.config/opencode/skills`,
  `~/.claude/skills`) on launch — **no extra setup step**. Provisioning is idempotent
  (silent when already current), marker-guarded (never clobbers a foreign same-named
  directory), and closes the gap left by `opencode-nono/install.sh`.
- An explicit **`omac setup`** command is also available to (re)provision **all**
  installed harnesses at once or to force-refresh after an upgrade — but it is not
  required for the everyday flow.
- No omac registration or activation is involved: omac deliberately ignores
  `SKILL.md`-only directories, and the harness surfaces the guidance skill directly.

## Capabilities

### New Capabilities
- `builtin-skill-provisioning`: omac ships the guidance-only `omac-write-a-skill`
  bundle inside the binary and provisions it into each installed harness's native
  skills directory, idempotently and without name clashes.

### Modified Capabilities
<!-- None: there is no openspec/specs/ archive yet, and no existing capability's
     requirements change. -->

## Impact

- **New embedded asset**: an `omac-write-a-skill` skill bundle baked into the omac
  binary.
- **CLI surface**: auto-provisioning hooked into `omac start`/`serve`, plus a new
  `omac setup` command for explicit all-harness / forced (re)provisioning.
- **Harness model**: a way to resolve each harness's native user-global skills dir
  (`internal/config`).
- **Does NOT touch** the registry, skill-config, keychain, or `serve` activation —
  guidance skills bypass all of that.
- **Docs**: README Quickstart + skills sections, and `CREATING_A_SKILL.md`'s
  relationship to the shipped `omac-write-a-skill` skill.
- **Retires** the external `opencode-nono/install.sh` skill-copy step as the way this
  skill reaches disk.
