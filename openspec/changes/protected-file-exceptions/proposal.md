# Protected-file exceptions for the mid-session watch

## Why

Creating a worktree in yarp3 generates several dotenv files at once, and the
mid-session protected-file watch reports every one of them. The popup-per-file
behavior is unusable noise. The watch is still valuable: a protected file
created after launch is readable by the agent until restart, and the user
should know that.

The first implementation (branch `NoRiceToday/Reduce-protected-file-noise`,
issue #438) batches the dialog and lets the user ignore one exact path or the
whole current batch. That solves repeated warnings for the same worktree, but
a new worktree under a new name has a new path and asks again. A global
basename exception ("ignore every `.env.local` everywhere") would solve it but
is not acceptable: `.env.local` commonly holds real secrets, and one click
would silence the warning in every project.

This change adds a folder-scoped basename exception. The folders come from the
session's launch path: the workspace folder and its parents, stopping before
`$HOME` and `/`. A session started in the repository folder covers future
worktrees under `.worktrees/<name>`. A session started inside a worktree can
pick a parent folder explicitly.

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
- **Project scope is acceptable:** an exception scoped to the project may be
  offered, as long as it cannot leak into unrelated projects.
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
- Add a folder-scoped basename exception, stored as `{root, name}` in
  `~/.config/omac/prefs.yaml`.
- Derive the folder candidates on the host from the session's launch path:
  the workspace folder first, then its parents, stopping before `$HOME` and
  `/`.
- Never read `.git`, never run git, and never trust `core.worktree` to choose
  the scope.
- Offer the folder choice only when a candidate contains the detected path.
  In practice that means any absolute launch path below `$HOME` or outside it
  but below `/`.
- Keep the dialog order narrow to broad:
  1. `Keep warning every time (default)`,
  2. `Suppress warnings for this path permanently: <path>` (one per file),
  3. `Suppress warnings for all N files permanently` (current batch),
  4. `Suppress warnings for every file <name> under <folder>` (one per unique
     basename per folder candidate).
- Do not add a global basename option, a config knob, or a pattern language.

## Options considered

| Option | Covers new worktrees | Security | Complexity | Verdict |
|---|---|---|---|---|
| Exact path only (current branch) | No | Narrow | Low | Keep as the narrow choice |
| Exact paths for the current batch | No | Narrow | Low | Keep as the bulk choice |
| Global basename | Yes | **Unacceptable**, silences secrets in every project | Low | Reject |
| Basename scoped to the launch folder | Yes, worktrees below it | Narrow to one folder tree | Low | Recommend |
| Basename scoped to the launch folder and its parents | Yes, and sibling worktrees when started inside one | Narrow to an explicitly shown folder | Low | Recommend |
| Basename scoped to the git root | Yes, all worktrees of the repo | Bare and forged-gitfile layouts can widen it | Medium | Reject (removed) |
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

- **`internal/cli/protected_watch.go`**: derive the folder candidates from the
  launch path, add the folder choice, and match paths against `{root, name}`.
  The dialog loop, exact-path persistence, verbose reporting, and audit
  behavior stay.
- **`internal/prefs/prefs.go`**: add a list of `{root, name}` exceptions next to
  `protected_watch_ignore`; both are user-global and written atomically.
- **Concurrency**: `prefs.Save` merges additive fields with the latest on-disk
  state under a shared file lock, so concurrent sessions cannot overwrite each
  other.
- **Docs**: update `docs/security.md` with the folder choice and the trade-off.
- **No new config keys** and no changes to the launcher config.

## Out of Scope / Future Work

- Revoking exceptions from the CLI. For now, delete the entry from
  `prefs.yaml`.
- Multi-select in the dialog. Each choice dismisses one file or one scope; the
  dialog reopens for the rest.
- Worktrees outside the launch tree (bare-repo layouts, `~/orca/workspaces`)
  need an exact-path or batch choice.
- Per-subdirectory scopes or glob patterns.
- Ignoring by content (for example "only files that look like templates").
- Automatic detection of "this file was created by the agent itself".
