# Design: project-scoped protected-file exceptions

## Decision

Keep the exact-path exception from #438 and add one broader choice: ignore a
basename inside one project, where "project" is the repository's git root
(`--git-common-dir`). Do not add a global basename option and do not add
configuration keys.

The git root is the only scope that satisfies both constraints:

- it covers every worktree of the same repository, including
  `.worktrees/<name>` with a name that did not exist at review time;
- it does not leak the exception into unrelated repositories.

## Dialog

Options, in order:

1. `Keep warning for N files` (default; Enter or cancel keeps it)
2. `Ignore this file permanently: <absolute path>` (one per listed file)
3. `Ignore all N files permanently` (current batch)
4. `Ignore every <name> in this project (<repo>)` (only when a root is known)

Option 4 is generated once per unique basename under the resolved root, at
most four names. It is last because it is the broadest choice. The label
contains the repository basename, and the dialog body can name the full root
so the user knows the scope before clicking.

The batch loop stays: after a per-file or per-project choice, the dialog
reopens with the remaining paths until the user keeps the warning or nothing
is left. A project choice removes only the paths under that root with that
name; paths outside the root stay in the list.

## Git-root resolution

Resolve the root in the parent, once per watch, only when a dialog is first
needed (no cost for quiet sessions).

1. Make the workdir absolute and resolve symlinks if possible.
2. Run, with a 2-second timeout:
   `git -C <workdir> rev-parse --path-format=absolute --git-common-dir`
   - clear `GIT_DIR`, `GIT_WORK_TREE`, and `GIT_COMMON_DIR` from the child
     environment so the user's shell cannot point the check at another repo;
   - set `GIT_TERMINAL_PROMPT=0`;
   - do not add `safe.directory` bypasses; a dubious-ownership refusal means
     no project option for that session.
3. The common dir is the main repository's `.git` for a linked worktree, so
   the project root is its parent. For a normal checkout, the same rule
   applies. For a submodule, the common dir lives under
   `.git/modules/<name>`; use `--show-toplevel` there. For a bare repository,
   there is no working root: no project option.
4. Canonicalise the root (`filepath.EvalSymlinks`, fallback to
   `filepath.Clean`).
5. **Containment check:** accept the root only if the workdir is the root or
   lies below it (`workdir == root || strings.HasPrefix(workdir, root+sep)`).
   This blocks a forged `.git` or `core.worktree` from scoping an exception to
   a different project's root. It also keeps matching simple: the detected
   path is always below the stored root, so `{root, name}` can match without
   extra git calls. A failed check means no project option. This covers the
   common `.worktrees/<name>` layout; a worktree outside the repository falls
   back to exact-path and batch choices.

Fallback: if `--git-common-dir` fails, try `git rev-parse --show-toplevel`.
It returns the linked worktree's own root, not the main repository, so it is
only useful for a normal checkout. It must pass the same containment check.

Edge cases:

- **Non-git project:** no project option. The exact-path choice remains.
- **Linked worktree:** `--git-common-dir` points at the main `.git`, so the
  root is the main repository and all worktrees share it.
- **Nested repository or submodule:** the resolved root is the nested repo,
  not the outer one. That keeps the scope honest; the user can review the
  outer repo separately.
- **Symlinked workdir:** canonicalise before comparing and storing.
- **Git not installed, timeout, or any error:** no project option.

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

- Exact list: cleaned path equality.
- Project list: `underRoot(path, root)` and `filepath.Base(path) == name`.
- `underRoot` compares canonical paths and requires the separator boundary, so
  `/src/yarp3-other` never matches a root of `/src/yarp3`.
- Dedupe on `(root, name)`. Clean and canonicalise the root on add.
- Missing `prefs.yaml` is an empty store; a write is atomic (existing
  implementation).

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
- The root is resolved on the host from git output and validated to contain
  the workdir. It is not taken from agent text or from the workdir config.
- The git command runs with a bounded timeout and a clean `GIT_*` environment.

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
    submodule;
  - non-git directory, bare repo, missing git, timeout;
  - a root that does not contain the workdir (forged `.git`) is rejected;
  - a worktree outside the repository gets no project option;
  - symlinked workdir resolves to the canonical root.
- Prefs:
  - exact and project lists round-trip;
  - `(root, name)` dedupe;
  - prefix boundary (`/src/yarp3-other` does not match `/src/yarp3`);
  - wrong name does not match;
  - symlink/canonical root matches.
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
