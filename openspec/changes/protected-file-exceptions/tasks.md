# Tasks

## 1. Baseline (branch `NoRiceToday/Reduce-protected-file-noise`, #438)

- [x] 1.1 Batch detections per scan tick and show one decision dialog.
- [x] 1.2 Add exact-path exceptions in `prefs.yaml` (`protected_watch_ignore`).
- [x] 1.3 Add the batch choice (`Ignore all N files permanently`).
- [x] 1.4 Keep the audit event and the facade protected-set entry for ignored
      paths.
- [x] 1.5 Collapse the session-end report to one summary line; list every
      path under `--verbose`.
- [x] 1.6 Fall back to the informational popup and notification when no dialog
      backend is available.

## 2. Git-root resolution

- [x] 2.1 Add a resolver that walks up from the workdir to the first `.git`
      entry and never reads `core.worktree` (it is agent-controlled and must
      not choose the scope).
- [x] 2.2 A `.git` directory maps to its parent. A `.git` file calls the shared
      `sandboxrun.ResolveWorktreeCommonDir`, which checks the gitdir
      back-pointer and the `<common>/worktrees/<name>` layout; a validated
      linked worktree maps to the main repository, while a submodule,
      separate git dir, or forged gitfile stays at the directory holding the
      file.
- [x] 2.3 Canonicalise the root and reject it unless the workdir is the root or
      lies below it. Refuse the filesystem root and the canonical `$HOME`; a
      shallow repo root such as `/repo` stays eligible. A worktree outside the
      repository gets no project option.
- [x] 2.4 Cache the result per watch; resolve lazily on the first dialog.
- [x] 2.5 Tests: normal repo, `core.worktree=<ancestor>`, linked worktree under
      `.worktrees/<name>`, linked worktree outside the repo, nested repo,
      submodule, non-git, bare repo, forged `.git`, repo at `$HOME`, root,
      shallow repo root, symlinked workdir.

## 3. Persistence

- [x] 3.1 Add `ProtectedWatchProjectIgnore{Root, Name}` and
      `ProtectedWatchProjectIgnores` to `internal/prefs`.
- [x] 3.2 Match a path when it is below the canonical root and its basename
      equals `Name`; enforce the separator boundary.
- [x] 3.3 Dedupe `(root, name)` and keep the atomic `0600` write.
- [x] 3.4 Tests: round-trip, dedupe, sibling root, wrong name, symlinked root,
      missing prefs file.

## 4. Dialog

- [x] 4.1 Add `Ignore every <name> in this project (<repo>)` only when the
      root is known and the path is under it.
- [x] 4.2 Order options: keep, exact paths, batch, project. The project option
      is last because it is the broadest.
- [x] 4.3 A project choice removes only the matching name under that root and
      leaves other names and other roots in the dialog.
- [x] 4.4 Never generate a global basename option.
- [x] 4.5 Tests: option order, presence/absence, persistence, a simulated new
      `.worktrees/<other>` path, no `all projects` label.

## 5. Docs and verification

- [x] 5.1 Update `docs/security.md` with the project scope, the git-root rule,
      and the trade-off.
- [x] 5.2 State that an ignored file stays protected at launch and in the audit
      log.
- [x] 5.3 Run `go build ./...`, `go vet`, and the focused unit tests; the full
      `internal/cli` suite needs a host that can write `/tmp` and
      `~/.config/omac`.
