---
title: Git service prototype
description: Opt-in Linux workflow with read-only Git metadata and permission-controlled local changes.
---

The Git service is an **experimental, opt-in Linux feature**, not the default
sandbox policy or a complete fix for [#542](https://github.com/TNG/oh-my-agentic-coder/issues/542).
It makes the enrolled checkout's entire `.git` directory read-only to the agent.
A built-in service performs a small set of local Git changes after permission
checks. Source files remain editable, so builds, tests and local Git inspection
can still run under the usual sandbox policy.

## Start a session

From an ordinary checkout, including an empty `git init` repository:

```sh
omac start --git-service=prototype \
  --git-user-name='Your Name' --git-user-email='you@example.com'
```

Or use `omac --workdir /absolute/path/to/repo start` with the same flags.
Identity must be supplied at launch; the service does not inherit your Git
identity, signing configuration, environment secrets or credential helpers.

Requirements:

- Linux with working bubblewrap network and process namespaces.
- Native ELF executables at `/usr/bin/git` and `/usr/bin/bwrap`; Git 2.43 or
  later in the 2.x series. Initial enforcement tests used Git 2.43.0,
  bubblewrap 0.11.0 and Linux 7.2.15. Distribution security updates remain
  necessary; these version checks do not certify a Git installation as secure.
- The built-in sandbox, a read-write checkout grant and no writable checkout
  ancestor or system-binary grants. Learn mode, `--no-sandbox`, external
  launchers and `--keep-running` are refused.
- A real `.git` directory, not a linked worktree, submodule or separate gitdir.
  Metadata symlinks, special files and multiply linked files are refused.
  For a locally hardlinked clone, make a fresh clone with `--no-hardlinks`.
  Metadata scans are limited to 100,000 entries.

The option currently works with `start`, not `continue`, `resume` or `serve`.
Launch fails rather than silently dropping the restriction if setup fails.

## Working inside the sandbox

```sh
# Edit source and run your normal build/tests first.
git status
git diff
omac git stage -- src/example.go
omac git commit -m 'Update example' --signoff
omac git branch topic
omac git switch topic
```

`stage` accepts explicit files, including deletions. Paths on the CLI are
relative to your current directory inside the enrolled checkout. It does not
support directories, `-A`, `.` or traversal into another repository. The service
refuses paths denied by the sandbox rather than staging denial-marker contents.

Directories containing protected files are fixed mount points during the session:
for example, if `deployment/local/.env` is protected, `deployment` and
`deployment/local` cannot be renamed or removed. Their other contents remain
editable, and unrelated directories can still be moved. This keeps the service's
deny paths valid instead of exposing a moved secret to a later Git worker.

`commit` commits the index, not unstaged edits. `--signoff` (or `-s`) adds a DCO
footer using the launch identity; it is **not** a cryptographic signature.
`branch` creates a local branch at HEAD without overwriting a branch or setting
an upstream. `switch` selects an existing local branch and requires a clean
index and worktree, including no untracked files. Ignored files are not silently
overwritten. Stop file-generating watchers and concurrent edits before switching.

Each modifying action has a separate permission. The existing approval dialog
can allow it once or for the session; denial or unavailable approval UI prevents
execution. **Approval covers the action in this project, not a particular diff
or commit message.** The dialog does not show request bodies. Read-only service
requests do not prompt.

Add `--git-wrapper` at launch to route these forms through the service:

```sh
git add -- src/example.go
git commit -m 'Update example' --signoff
git branch topic
git switch topic
```

The wrapper also passes `status`, `log`, `diff`, `show`, `rev-parse`, `ls-files`
and `ls-tree` to local Git. Other syntax fails; full Git CLI compatibility is
not a goal. Bypassing the wrapper with `/usr/bin/git` or a library does not
bypass the kernel's read-only metadata mount. Local reads use the agent's
ordinary configuration and sandbox, not the service worker's configuration.

## Unsupported workflows

No fetch, push, configuration edits, hooks, signing, LFS/custom filters,
working-tree encodings, merge/rebase/cherry-pick, stash/reset/clean, interactive
Git, detached HEAD, submodules or linked worktrees. Sparse, shallow, partial,
bare, non-SHA-1 and alternative-object-store repositories are refused, as are
in-progress merge/rebase states and unsupported repository extensions.

Local configuration includes and external attributes files are refused rather
than interpreting them differently. Case-insensitive checkouts, disabled symlink
support, CRLF configuration and automatic line-ending conversion are unsupported.
The service preserves `core.filemode`; global/system configuration is ignored.
Repositories relying on global settings or ignores may therefore behave
differently. Tracked `.gitattributes` still apply, but filter/encoding attributes
are rejected before staging or checking out affected files.

Use ordinary host Git for unsupported operations **after ending the session and
reviewing the changes**. Do not run concurrent host Git mutations or another agent session against this
checkout. The service's lock excludes another service instance, not ordinary
host Git or agents launched without this option.

## Failure recovery

Results include the session, HEAD, branch and an index fingerprint. Mutations
carry an operation ID and expected state. Duplicate requests with the same ID
and body return their recorded result, not a second commit. IDs are valid only
for the current session; the ledger holds at most 4,096 operations.

After a lost response, use the ID printed by the client:

```sh
omac git operation SESSION:NONCE
omac git status
```

Do not blindly resubmit using a new ID. The service finishes independently of a
client disconnect, with a 30-second operation deadline. If an operation may have
partly changed the repository, it reports `outcome_unknown` and refuses further
mutations. Inspect native Git state, end the session and relaunch. There is no
automatic checkout rollback, lock-file deletion or durable replay across crashes.
An unknown ID after a restart does not prove the old operation never ran.

## Security boundary and limits

The controller is part of the launcher, not a project-editable skill. Its
reserved `/git` facade route uses the existing authentication and approval gates;
skill reload cannot replace it. Its approval identity cannot be claimed by a
project skill. `omac git` prefers the permission-protected
`OMAC_SOCKET` transport and otherwise uses the loopback `OMAC_GIT_BASE` with the
facade token. HTTP redirects are not followed.

Every service Git command runs in its own kernel sandbox with no network,
host credentials or inherited agent environment. The worker sees a private
HOME, temporary storage and generated configuration mounted over `.git/config`.
The original config and hooks are not rewritten. Hooks, configured helper
commands, signing and automatic maintenance are disabled. Metadata is writable
only for mutation workers; source files are writable only for branch switching.
Operation types, result status and commit IDs are audited, not file contents or
commit messages.

This protects **one enrolled checkout only**. Other granted repositories,
nested repositories and repos initialized during the session are not protected
by this feature. Writable bind-mount aliases visible in the launch mount table are refused.
Other unusual filesystem alias layouts are outside the prototype's supported
setup; use an ordinary checkout without aliases. Secret-path masks are resolved at launch, with the same
new-file limitations as the ordinary Linux sandbox.

**Existing host hooks can still execute agent-edited project scripts.** For
example, protecting `.git/config` does not protect a tracked `.githooks/test.sh`
that an already-configured host hook invokes. The service can switch and edit
such scripts without executing them, but it does not certify their future host
execution as safe. Review them before running host Git. Independent security
and workflow review is required before expanding this prototype's scope.
