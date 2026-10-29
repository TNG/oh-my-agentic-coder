# Protected-file exceptions for the mid-session watch

## Why

Creating a worktree in yarp3 generates several dotenv files at once, and the
mid-session protected-file watch reports every one of them. The current
popup-per-file behavior is unusable noise. The watch is still valuable: a
protected file created after launch is readable by the agent until restart,
and the user should know that.

The first implementation (branch `NoRiceToday/Reduce-protected-file-noise`,
issue #438) batches the dialog and lets the user ignore one exact path or the
whole current batch. That solves repeated warnings for the same worktree, but
a new worktree under a new name has a new path and asks again. A global
basename exception ("ignore every `.env.local` everywhere") would solve it but
is not acceptable: `.env.local` commonly holds real secrets, and one click
would silence the warning in every project.

This change defines the option space and recommends a project-scoped
exception keyed by the repository's git root. The git root is the same for all
worktrees of one repository, so a new worktree under `.worktrees/<name>` is
covered, while other projects keep the warning.

## Original requirements

These are the requirements the design has to satisfy, in the words of the
originating user request:

- **Noise source:** creating new worktrees in yarp3 produces many warnings for
  generated files such as
  `common/scripts/local-dev/local-env/files/web-ui/.env.local`.
- **Per-entry choice:** the dialog should let the user choose an exception for
  individual entries or for all entries, similar to the network-access prompt.
- **Persisted exception:** choosing an exception must persist for the concrete
  file, not only for the current session.
- **Not global:** a global basename option is not acceptable and must not be
  offered.
- **Project scope is acceptable:** an exception scoped to the project, using
  the git root, may be offered.
- **KISS:** no configuration matrix and no pattern language. The dialog is the
  only place where an exception is created.
- **Keep protection:** an ignored file stays protected at launch. Only the
  repeated mid-session warning is suppressed.
- **Keep the audit trail:** every detection stays in the audit log, including
  ignored files.
- **Intuitive order:** the safe choice is the default, and the options are
  ordered from narrow to broad.
- **No dialog backend:** without a desktop dialog, omac falls back to the
  informational popup and notification.

## What Changes

- Keep the batch dialog and the exact-path exception from #438.
- Add a project-scoped basename exception, stored as `{root, name}` in
  `~/.config/omac/prefs.yaml`.
- Resolve `root` on the host from the workdir's git common directory, so a
  linked worktree maps to the main repository root.
- Offer `Ignore every <name> in this project` only when the root can be
  resolved and contains the detected path. Do not offer it for non-git
  projects or when git returns a root outside the workdir.
- Keep the dialog order narrow to broad:
  1. `Keep warning for N files` (default),
  2. `Ignore this file permanently: <path>` (one per file),
  3. `Ignore all N files permanently` (current batch),
  4. `Ignore every <name> in this project` (only when a root is known).
- Do not add a global basename option, a config knob, or a pattern language.

## Options considered

| Option | Covers new worktrees | Security | Complexity | Verdict |
|---|---|---|---|---|
| Exact path only (current branch) | No | Narrow | Low | Keep as the narrow choice |
| Exact paths for the current batch | No | Narrow | Low | Keep as the bulk choice |
| Global basename | Yes | **Unacceptable**, silences secrets in every project | Low | Reject |
| Basename scoped to the session workdir | No, worktrees have different workdirs | Narrow | Low | Reject |
| Basename scoped to the git root (recommended) | Yes, all worktrees of the repo | Project-wide name only | Medium | Propose |
| Config `ignore` / `project_ignore` globs | Yes | Project config is agent-writable; global config is a matrix | Medium | Reject (tried and removed) |
| Automatically ignore `.env.local` / `.env.example` | Yes | Silences a common secrets file | Low | Reject |
| Session-wide "ignore this name" (no persistence) | Only until restart | Narrow in time, broad in name | Low | Could be a future addition |

## Capabilities

### New Capabilities

- `protected-file-exceptions`: the dialog choices, their persistence, and the
  rules that decide which paths are skipped. This is an extension of the
  existing mid-session watch described in `docs/security.md`.

### Modified Capabilities

<!-- none: openspec/specs/ has no baseline spec for the mid-session watch -->

## Impact

- **`internal/cli/protected_watch.go`**: add git-root resolution, the project
  choice, and matching. The dialog loop, exact-path persistence, verbose
  reporting, and audit behavior stay.
- **`internal/prefs/prefs.go`**: add a list of `{root, name}` exceptions next to
  `protected_watch_ignore`; both are user-global and written atomically.
- **New helper**: a git-root resolver with a bounded command timeout and a
  containment check. It runs in the parent (host), never in the sandbox.
- **Docs**: update `docs/security.md` with the project option and the trade-off.
- **No new config keys** and no changes to the launcher config.

## Out of Scope / Future Work

- Revoking exceptions from the CLI. For now, delete the entry from
  `prefs.yaml`.
- Per-subdirectory scopes or glob patterns.
- Ignoring by content (for example "only files that look like templates").
- Automatic detection of "this file was created by the agent itself".
