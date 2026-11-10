# Tasks

## 1. Baseline (branch `NoRiceToday/Reduce-protected-file-noise`, #438)

- [x] 1.1 Batch detections per scan tick and show one decision dialog.
- [x] 1.2 Add exact-path exceptions in `prefs.yaml` (`protected_watch_ignore`).
- [x] 1.3 Add the batch choice (`Suppress warnings for all N files
      permanently`).
- [x] 1.4 Keep the audit event and the facade protected-set entry for
      suppressed paths.
- [x] 1.5 Collapse the session-end report to one summary line; list every
      path under `--verbose`.
- [x] 1.6 Fall back to the informational popup and notification when no dialog
      backend is available.

## 2. Folder scope from the launch path

- [x] 2.1 Derive candidates lexically from the session workdir: the workspace
      folder first, then its parents.
- [x] 2.2 Stop before the canonical `$HOME` and the filesystem root; reject an
      empty or relative workdir.
- [x] 2.3 Never read `.git`, never run git, and never trust `core.worktree` for
      the scope.
- [x] 2.4 Replace the earlier git-root resolver and its tests; a forged `.git`
      or worktree admin dir can no longer widen the first folder.
- [x] 2.5 Tests: unsafe workdirs offer nothing; a worktree launch path offers
      the workspace, `.worktrees`, and the repository root in order; metadata
      cannot change the first scope.

## 3. Persistence

- [x] 3.1 Add `ProtectedWatchProjectIgnore{Root, Name}` and
      `ProtectedWatchProjectIgnores` to `internal/prefs`.
- [x] 3.2 Match a path when it is below the canonical root and its basename
      equals `Name`; enforce the separator boundary.
- [x] 3.3 Dedupe `(root, name)` and keep the atomic `0600` write.
- [x] 3.4 Merge additive preferences with the latest on-disk state under a
      shared lock in `prefs.Save`, so concurrent sessions keep each other's
      exceptions and `suppress_plugin_warning`.
- [x] 3.5 Tests: round-trip, dedupe, sibling root, wrong name, symlinked root,
      missing prefs file, concurrent saves.

## 4. Dialog

- [x] 4.1 Add `Suppress warnings for every file <name> under <folder>` for each
      unique basename per folder candidate, at most four names per folder.
- [x] 4.2 Order options: keep, exact paths, batch, folder. The folder options
      are last because they are the broadest.
- [x] 4.3 A folder choice removes only the matching name under that folder and
      leaves other names and other folders in the dialog.
- [x] 4.4 Never generate a global basename option.
- [x] 4.5 Keep the prompt short: what was detected, that the file is readable
      until restart, and that suppressing keeps protection and audit.
- [x] 4.6 Size the generic dialog and disable markup so long paths with `&`,
      `<`, or `>` render and round-trip unchanged.
- [x] 4.7 Return `ctx.Err()` from the macOS dialog on timeout, matching zenity
      and kdialog, so a timeout does not raise the no-backend alert.
- [x] 4.8 Tests: option order, presence/absence, persistence, a simulated new
      `.worktrees/<other>` path, no `all projects` label.

## 5. Docs and verification

- [x] 5.1 Update `docs/security.md` with the folder scope, the launch-path
      rule, and the trade-off.
- [x] 5.2 State that a suppressed file stays protected at launch and in the
      audit log.
- [x] 5.3 Keep this change's `design.md`, `proposal.md`, `spec.md`, and
      `tasks.md` aligned with the implemented behavior.
- [x] 5.4 Run `go build ./...`, `go vet`, and the focused unit tests; the full
      `internal/cli` suite needs a host that can write `/tmp` and
      `~/.config/omac`.

## 6. Deferred

- [ ] 6.1 Multi-select in the dialog. The native backends are single-choice;
      a checkbox variant needs a new API on osascript, zenity, and kdialog.
- [ ] 6.2 `omac prefs` for revoking exceptions. For now, edit `prefs.yaml`.
