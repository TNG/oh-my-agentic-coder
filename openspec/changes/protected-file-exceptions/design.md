# Design: project-scoped protected-file exceptions

## Decision

Keep the exact-path exception from #438 and add one broader choice: ignore a
basename inside one project, where "project" is the repository root found by
walking up from the workdir to the first `.git` entry. Do not add a global
basename option and do not add configuration keys.

The git root is the only scope that satisfies both constraints:

- it covers every worktree of the same repository, including
  `.worktrees/<name>` with a name that did not exist at review time;
- it does not leak the exception into unrelated repositories.

## Dialog

Options, in order:

1. `Keep warning for N files (report at exit)` (default; Enter or cancel
   keeps it, saves no exception, and lists the files in the session-end report)
2. `Ignore this file permanently: <absolute path>` (one per listed file)
3. `Ignore all N files permanently` (current batch, with `(8 listed)` when
   the dialog showed only the first eight)
4. `Ignore every <name> in this project (<repo>)` (only when a root is known)

Option 4 is generated once per unique basename under the resolved root, at
most four names. It is last because it is the broadest choice. The label
contains the repository basename, and the dialog body can name the full root
so the user knows the scope before clicking.

The batch loop stays: after a per-file or per-project choice, the dialog
reopens with the remaining paths until the user keeps the warning or nothing
is left. A project choice removes only the paths under that root with that
name; paths outside the root stay in the list. The 10-second scan pauses while
a dialog is open; a file created then is detected on the next tick after the
dialog closes, except one created and deleted while it is open.

## Git-root resolution

Resolve the root in the parent, once per watch, only when a dialog is first
needed (no cost for quiet sessions). Discovery reads the filesystem, not git
output, because `core.worktree` is agent-controlled: `git rev-parse
--show-toplevel` honours it, and a config file the sandboxed agent can write
must never choose the scope.

1. Make the workdir absolute and canonicalise it (`EvalSymlinks` with a
   re-graft of the missing tail, shared as `pathutil.Canonical`).
2. Walk up from the workdir to the first `.git` entry. If none is found (a
   non-git directory, a bare repository), there is no project option.
3. If `.git` is a directory, the root is its parent. `core.worktree` is
   ignored entirely.
4. If `.git` is a file, call the shared `sandboxrun.ResolveWorktreeCommonDir`.
   When it validates a linked worktree (gitdir back-pointer plus the
   `<common>/worktrees/<name>` layout invariant), the root is the parent of
   the returned common dir, so every worktree maps to the main repository.
   When it does not validate (a submodule, a separate git dir, a forged
   gitfile), the root is the directory that contains the `.git` file. Never
   use an ancestor above that directory.
5. **Containment check:** accept the root only if the canonical workdir is the
   root or lies below it (`pathutil.Under`). This blocks a forged `.git` or
   `core.worktree` from scoping an exception to a different project's root.
6. **Breadth gates:** refuse the filesystem root and the user's home directory
   (canonical `$HOME`). These are the two scopes that would silently cover
   unrelated projects. A shallow repo root such as `/repo` or `/app` stays
   eligible: discovery already anchors it at the directory that owns `.git`,
   so a depth threshold would only add an arbitrary false negative.

A failed check means no project option, and there is no fallback to a broader
main-repo-wide scope. A worktree outside the repository also gets no project
option because the main root does not contain it.

Edge cases:

- **Non-git project:** no project option. The exact-path choice remains.
- **Linked worktree:** the validated common dir is the main `.git`, so the
  root is the main repository and all worktrees share it.
- **Nested repository or submodule:** the root is the nested repo or the
  submodule's own directory, not the outer one. That keeps the scope honest;
  the user can review the outer repo separately.
- **Symlinked workdir:** canonicalise before comparing and storing.
- **Bare repository:** no working root: no project option.

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
    ProtectedWatchIgnore         []string                     `yaml:"protected_watch_ignore"`
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
- Missing `prefs.yaml` is an empty store; a write is atomic (existing
  implementation). An unreadable `prefs.yaml` is not replaced: the watch keeps
  exceptions in memory for the session and records one notice that they will
  not be saved.

Both lists are user-global and live under `~/.config/omac`, which the sandbox
blocks. The agent can create the file that triggers a warning, but it cannot
grant itself an exception.

## Security

- The exception suppresses the **notice only**. The protected set, the
  launch-time mask, and the audit event are unchanged. A new file in that
  project/name can still be read by the agent until restart, and
  `GET /sandbox/denied` still reports it honestly.
- Project scope is a deliberate trade-off: one click covers any future
  `.env.local` under that repository, including files created by another
  process. The dialog label names the repository so the user can see that.
- No global basename option exists. A user cannot accidentally silence a name
  in every project from this dialog.
- The root is discovered on the host by walking the filesystem to the first
  `.git` entry and validating a linked worktree's admin dir. It is not taken
  from agent text and not from the workdir config: `core.worktree` is ignored.
- The root must contain the workdir, and the filesystem root and the user's
  home directory are refused.

## Rejected alternatives

- **Global basename:** unacceptable; `.env.local` commonly holds secrets and
  one click would silence every project.
- **Session-workdir scope:** a new worktree is a new workdir, so it does not
  solve the noise.
- **`git rev-parse --show-toplevel` as the primary source:** for a linked
  worktree it returns the worktree root, not the main repository root.
- **Strip `/worktrees/<name>` from the git dir:** only works for one layout
  and silently breaks for custom worktree locations.
- **Path globs in config:** more configuration and a pattern language; the
  previous `ignore` / `project_ignore` attempt was removed for that reason.
- **Auto-ignore `.env.local` / `.env.example`:** `.env.example` is already not
  protected by default; `.env.local` can hold secrets. A default silence would
  hide a real leak.
- **Per-project config file:** the workdir config is writable by the agent, so
  it must not be able to grant itself exceptions.

## Test plan

- Git root:
  - normal repo, linked worktree under `.worktrees/<name>`, nested repo,
    submodule, symlinked workdir;
  - non-git directory, bare repo;
  - a `.git` directory whose config sets `core.worktree=<broad ancestor>`
    resolves to the repo dir, never the ancestor;
  - a forged `.git` file pointing at an agent-controlled admin dir resolves
    to the workdir (or no root), never an ancestor;
  - a repo at `$HOME` or `/` is refused; a shallow root like `/repo` is accepted
    both in the main checkout and in its worktree;
  - a root that does not contain the workdir (forged `.git`) is rejected;
  - a worktree outside the repository gets no project option.
- Prefs:
  - exact and project lists round-trip;
  - a batch of exact paths saves once;
  - `(root, name)` dedupe;
  - prefix boundary (`/src/yarp3-other` does not match `/src/yarp3`);
  - wrong name does not match;
  - symlink/canonical root matches;
  - an exact path stored under a symlinked spelling matches the real path.
- Watch/dialog:
  - options order: keep, exact files, batch, project;
  - project option absent without a root;
  - project option names the repository and persists `(root, name)`;
  - selecting it skips a second path under the same root with the same name,
    including a simulated new `.worktrees/<other>` path;
  - paths outside the root stay in the remaining list;
  - no option contains "all projects";
  - audit still emits one event per detected path;
  - `--verbose` lists only the warnings the user kept;
  - session-end summary excludes ignored paths;
  - no dialog backend falls back to the informational popup and notification.

## Open questions

1. **Non-git projects:** exact path only (recommended) or treat the session
   workdir as a pseudo-root? A pseudo-root would not cover a new sibling
   directory, so it adds a scope that is easy to misread.
2. **Submodules:** scope to the submodule root (recommended) or map to the
   outer repository? The former is what git reports and keeps the exception
   narrow.
3. **Label detail:** repository basename in the option plus the full root in
   the dialog body (recommended), or the full root in the option label?
4. **Revocation:** edit `prefs.yaml` for now, or add `omac prefs` in a later
   change?
