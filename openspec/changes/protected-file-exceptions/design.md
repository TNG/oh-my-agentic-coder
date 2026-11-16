# Design: folder-scoped protected-file exceptions

## Decision

Keep the exact-path and batch exceptions from #438 and add one broader choice:
suppress the warning for every file with a given basename below a folder. The
folder candidates come only from the session's launch path: the workspace
folder first, then its parents, stopping before the user's home directory and
the filesystem root.

Do not parse `.git`, do not run git, and do not add a global basename option or
configuration keys.

The launch path is the only scope that is both useful and trustworthy:

- the noise in #438 is worktrees created under the folder the session runs in;
- the agent cannot widen the path, unlike a `.git` file or `core.worktree`,
  which it can write;
- one folder choice covers future worktrees below that folder, so a new
  `.worktrees/<name>` does not ask again.

Trade-off: a worktree outside the launch tree, for example Orca's default
`~/orca/workspaces` or a bare-repo layout in a sibling folder, is not covered
by one exception. The user can still suppress an exact path or the batch.

## Dialog

Options, in order:

1. `Keep warning every time (default)` — Enter or cancel keeps the warning,
   saves no exception, and lists the files in the session-end report.
2. `Suppress warnings for this path permanently: <absolute path>` — one per
   listed file.
3. `Suppress warnings for all N files permanently` — the current batch, with
   `(8 listed)` appended when the dialog showed only the first eight.
4. `Suppress warnings for every file <name> under <folder>` — one per unique
   basename per folder candidate, at most four names per folder.

The keep option is first because the dialog backends use the first entry as
the default, so pressing Enter never grants an exception. The folder options
are last because they are the broadest.

The dialog is single-choice. After a per-file or per-folder choice it reopens
with the remaining paths until the user keeps the warning or nothing is left.
Multiple selections in one pass would need a new dialog API on every platform
and are out of scope for this change.

The 10-second scan pauses while a dialog is open; a file created then is
detected on the next tick after the dialog closes, except one created and
deleted while it is open.

## Folder candidates

`protectedWorkspaceRoots` derives the candidates lexically:

1. Reject an empty or relative workdir.
2. Start at `filepath.Clean(workdir)` and walk to its parents.
3. Stop before the canonical `$HOME` and before the filesystem root. Neither
   is an appropriate scope: one click there would silence unrelated projects.
4. Change nothing on disk and read no `.git` entry, so the candidates cannot
   move when the agent writes files.

Result for a session started in `/home/me/repo/.worktrees/A`:

```
/home/me/repo/.worktrees/A
/home/me/repo/.worktrees
/home/me/repo
```

A session started in `/home/me/repo` offers only `/home/me/repo`. A session
started directly in `/` or `$HOME` offers no folder option.

The first candidate is always the launch folder, so it is safe to assume it
appears first: a `.git` file, a forged admin dir, or `core.worktree` cannot
change it. Choosing a parent is an explicit user action.

## Storage

Add one list to `~/.config/omac/prefs.yaml` next to the exact paths:

```yaml
protected_watch_ignore:
  - /abs/path/to/worktree/common/.../web-ui/.env.local
protected_watch_project_ignores:
  - root: /Users/me/src/yarp3
    name: .env.local
```

Go shape:

```go
type ProtectedWatchProjectIgnore struct {
    Root string `yaml:"root"`
    Name string `yaml:"name"`
}

type Store struct {
    // ...
    ProtectedWatchIgnore         []string                      `yaml:"protected_watch_ignore"`
    ProtectedWatchProjectIgnores []ProtectedWatchProjectIgnore `yaml:"protected_watch_project_ignores"`
}
```

Matching rules:

- Exact list: canonical path equality (shared `pathutil.Canonical`), so a
  symlinked spelling of the same file does not prompt again.
- Project list: `pathutil.Under(root, path)` and `filepath.Base(path) == name`.
- `pathutil.Under` compares canonical paths with the separator boundary, so
  `/src/yarp3-other` never matches a root of `/src/yarp3`.
- Dedupe on `(root, name)` and on the exact path. Canonicalise the root and
  the exact paths on add.
- Missing `prefs.yaml` is an empty store; a write is atomic (`0600`). An
  unreadable `prefs.yaml` is not replaced: the watch keeps exceptions in memory
  for the session and records one notice that they will not be saved.
- `prefs.Save` merges additive fields with the latest on-disk state under a
  shared file lock, so a second session cannot overwrite another session's
  exceptions or `suppress_plugin_warning`.

Both lists are user-global and live under `~/.config/omac`, which the sandbox
blocks. The agent can create the file that triggers a warning, but it cannot
grant itself an exception.

## Security

- The exception suppresses the **notice only**. The protected set, the
  launch-time mask, and the audit event are unchanged. A new file covered by
  an exception can still be read by the agent until restart, and
  `GET /sandbox/denied` still reports it honestly.
- Folder scope is a deliberate trade-off: one click covers any future
  `.env.local` below that folder, including files created by another process.
  The label shows the full path so the user can see what is covered.
- Choosing a parent folder also covers other projects below it. The dialog
  body says so. The workspace folder is offered first, so the narrow choice
  is the easiest one.
- No global basename option exists. A user cannot silence a name in every
  project from this dialog.
- The scope never comes from `.git`, `core.worktree`, or git output. A forged
  worktree link can only change what git reports, which this design does not
  read.
- The filesystem root and the canonical home directory are refused.

## Rejected alternatives

- **Git-root resolution:** removed. A bare repository resolves to the parent
  of the bare directory, so `~/repos/shop.git` would scope the exception to
  every repository under `~/repos`. A forged `.git` file plus a matching
  `<x>/worktrees/<name>` admin dir can make both sides of the validation
  agent-written, so the checks all pass. The scope was also harder to reason
  about than the launch path.
- **Global basename:** unacceptable; `.env.local` commonly holds secrets and
  one click would silence every project.
- **`git rev-parse --show-toplevel`:** for a linked worktree it returns the
  worktree root, and `core.worktree` is agent-controlled.
- **Strip `/worktrees/<name>` from the git dir:** only works for one layout and
  silently breaks for custom worktree locations.
- **Workspace folder only, without parents:** a session started inside
  `.worktrees/A` could not cover sibling worktrees. The parents are free to
  offer because they come from the same path string, and the label shows the
  full path.
- **Path globs in config:** more configuration and a pattern language; the
  previous `ignore` / `project_ignore` attempt was removed for that reason.
- **Auto-ignore `.env.local` / `.env.example`:** `.env.example` is already not
  protected by default; `.env.local` can hold secrets. A default silence would
  hide a real leak.
- **Per-project config file:** the workdir config is writable by the agent, so
  it must not be able to grant itself exceptions.

## Test plan

- Folder candidates:
  - empty, relative, `/`, and `$HOME` offer no option;
  - a workspace below `$HOME` offers itself and its parents, stopping before
    `$HOME`;
  - a workspace outside `$HOME` still stops before `/`.
- Prefs:
  - exact and project lists round-trip;
  - a batch of exact paths saves once;
  - `(root, name)` dedupe;
  - prefix boundary (`/src/yarp3-other` does not match `/src/yarp3`);
  - wrong name does not match;
  - symlink/canonical root matches;
  - an exact path stored under a symlinked spelling matches the real path;
  - concurrent saves keep both sessions' exceptions.
- Watch/dialog:
  - options order: keep, exact files, batch, folder;
  - folder options run from workspace to parent, stopping before `$HOME`;
  - a workspace choice does not cover a sibling project, a parent choice does;
  - a forged `.git` next to the workspace cannot change the first folder
    scope;
  - selecting a folder skips a second path with the same name below it,
    including a simulated new `.worktrees/<other>` path;
  - paths outside the root stay in the remaining list;
  - no option contains "all projects";
  - audit still emits one event per detected path;
  - `--verbose` lists only the warnings the user kept;
  - session-end summary excludes suppressed paths;
  - no dialog backend falls back to the informational popup and notification.

## Open questions

1. **Multi-select:** the native dialogs are single-choice. A checkbox variant
   would let the user pick several exceptions in one pass; it needs a new API
   on osascript, zenity, and kdialog and is deferred.
2. **Worktrees outside the launch tree:** a bare-repo layout or
   `~/orca/workspaces` is not covered by one folder choice. The exact-path and
   batch choices remain. Revisit if this becomes a common complaint.
3. **Revocation:** edit `prefs.yaml` for now, or add `omac prefs` in a later
   change?
