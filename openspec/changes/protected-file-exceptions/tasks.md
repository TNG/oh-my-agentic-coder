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

- [ ] 2.1 Add a resolver that runs
      `git -C <workdir> rev-parse --path-format=absolute --git-common-dir`
      with a bounded timeout, a clean `GIT_*` environment, and
      `GIT_TERMINAL_PROMPT=0`.
- [ ] 2.2 Map the common git dir to the main repository root, with
      `--show-toplevel` only as a fallback for a normal checkout.
- [ ] 2.3 Canonicalise the root and reject it unless the workdir is the root or
      lies below it. That containment check is the security boundary.
- [ ] 2.4 Cache the result per watch; resolve lazily on the first dialog.
- [ ] 2.5 Tests: normal repo, linked worktree, nested repo/submodule, non-git,
      bare repo, missing/failing/timeout git, forged root outside the workdir.

## 3. Persistence

- [ ] 3.1 Add `ProtectedWatchProjectIgnore{Root, Name}` and
      `ProtectedWatchProjectIgnores` to `internal/prefs`.
- [ ] 3.2 Match a path when it is below the canonical root and its basename
      equals `Name`; enforce the separator boundary.
- [ ] 3.3 Dedupe `(root, name)` and keep the atomic `0600` write.
- [ ] 3.4 Tests: round-trip, dedupe, sibling root, wrong name, symlinked root,
      missing prefs file.

## 4. Dialog

- [ ] 4.1 Add `Ignore every <name> in this project (<repo>)` only when the
      root is known and the path is under it.
- [ ] 4.2 Order options: keep, exact paths, batch, project. The project option
      is last because it is the broadest.
- [ ] 4.3 A project choice removes only the matching name under that root and
      leaves other names and other roots in the dialog.
- [ ] 4.4 Never generate a global basename option.
- [ ] 4.5 Tests: option order, presence/absence, persistence, a simulated new
      `.worktrees/<other>` path, no `all projects` label.

## 5. Docs and verification

- [ ] 5.1 Update `docs/security.md` with the project scope, the git-root rule,
      and the trade-off.
- [ ] 5.2 State that an ignored file stays protected at launch and in the audit
      log.
- [ ] 5.3 Run `go build ./...`, `go vet`, and the focused unit tests; the full
      `internal/cli` suite needs a host that can write `/tmp` and
      `~/.config/omac`.
