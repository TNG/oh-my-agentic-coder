# protected-file-exceptions Specification

## Purpose

Define which mid-session protected-file warnings the user can dismiss, at what
scope, where the exception is stored, and what stays protected. The watch is
described in `docs/security.md`; this capability only adds the exception
choices and their persistence.

## Requirements

### Requirement: Exact-path exceptions

The dialog SHALL let the user dismiss the warning for one detected file by its
exact absolute path, and SHALL persist that path in the user-global prefs.

#### Scenario: Suppress one file

- **WHEN** the dialog lists `/repo/.worktrees/a/.env.local` and the user picks
  `Suppress warnings for this path permanently`
- **THEN** that exact path is stored under `protected_watch_ignore`
- **AND** the same path is not reported again in this or a later session
- **AND** the file remains protected at launch and is still audit-logged

#### Scenario: Exact path does not cover a sibling

- **WHEN** the user suppressed `/repo/.worktrees/a/.env.local`
- **AND** `/repo/.worktrees/b/.env.local` is created later
- **THEN** the second path is still reported

### Requirement: Batch exception

The dialog SHALL let the user dismiss every file listed in the current batch in
one choice.

#### Scenario: Suppress the current batch

- **WHEN** the dialog lists three paths and the user picks
  `Suppress warnings for all 3 files permanently`
- **THEN** all three exact paths are stored in `protected_watch_ignore`
- **AND** none of them is reported again in this session
- **AND** a fourth path created later is still reported

### Requirement: Folder-scoped name exception

The dialog SHALL offer a folder-scoped basename exception derived only from the
session's launch path. The candidates SHALL be the workspace folder first, then
its parents, stopping before the canonical `$HOME` and the filesystem root.

#### Scenario: New worktree below the workspace folder

- **WHEN** the session started in `/repo`
- **AND** the batch contains `/repo/.worktrees/a/.env.local`
- **AND** the user picks
  `Suppress warnings for every file .env.local under /repo`
- **THEN** `{root: /repo, name: .env.local}` is stored under
  `protected_watch_project_ignores`
- **AND** a later `/repo/.worktrees/b/.env.local` is not reported
- **AND** `/repo/.env.local` is not reported

#### Scenario: Session started inside a worktree

- **WHEN** the session started in `/repo/.worktrees/a`
- **THEN** the dialog offers `/repo/.worktrees/a`, `/repo/.worktrees`, and
  `/repo`, in that order
- **AND** choosing `/repo` also covers a later `/repo/.worktrees/b/.env.local`
- **AND** choosing `/repo/.worktrees/a` leaves sibling worktrees reported

#### Scenario: Another folder keeps the warning

- **WHEN** the user stored `{root: /repo, name: .env.local}`
- **AND** `/other-repo/.env.local` is created
- **THEN** `/other-repo/.env.local` is still reported

#### Scenario: Folder boundary is exact

- **WHEN** the stored root is `/repo`
- **AND** a path is `/repo-other/.env.local`
- **THEN** the folder exception does not match

#### Scenario: No folder option at an unsafe root

- **WHEN** the launch path is `/`, the canonical `$HOME`, empty, or relative
- **THEN** the dialog does not offer a folder-scoped option
- **AND** the exact-path and batch options remain

#### Scenario: Git metadata cannot choose the scope

- **WHEN** the workdir contains a `.git` file and matching worktree admin
  files written by the agent
- **THEN** the first folder option is still the workspace folder
- **AND** no `.git` content changes the candidate list

#### Scenario: Folder choice does not hide other names

- **WHEN** the batch contains `.env.local` and `.env` under the same folder
- **AND** the user picks the `.env.local` folder option
- **THEN** only the `.env.local` paths under that folder are dismissed
- **AND** the `.env` path stays in the dialog / report

### Requirement: No global basename exception

The dialog SHALL NOT offer an exception that applies a basename to every
project.

#### Scenario: No global option

- **WHEN** the dialog is shown for any batch
- **THEN** no option label contains `all projects`
- **AND** no stored exception matches a basename without a root

### Requirement: Exceptions are user-global and outside the sandbox

Exceptions SHALL be stored in `~/.config/omac/prefs.yaml`, atomically and with
mode `0600`, and SHALL NOT be writable by the sandboxed agent.

#### Scenario: Agent cannot grant itself an exception

- **WHEN** the sandboxed agent writes files in the workdir
- **THEN** it cannot modify `~/.config/omac/prefs.yaml`
- **AND** it cannot create a folder-scoped exception without the user's dialog
  choice

### Requirement: Concurrent sessions merge preferences

Saving exceptions SHALL merge with the latest on-disk state under a shared file
lock, so one session cannot remove another session's exception or re-enable a
suppressed plugin warning.

#### Scenario: Two sessions save at the same time

- **WHEN** session A and session B each store a different exception
- **THEN** both exceptions are present in `prefs.yaml` afterwards

### Requirement: Protection and audit are unchanged

An exception SHALL suppress only the notice. The path SHALL remain in the
facade's protected set, SHALL remain masked at launch when it exists then, and
SHALL still produce one audit event per detection.

#### Scenario: Suppressed file is still audited and readable

- **WHEN** a path matches the folder exception
- **THEN** no dialog is shown for it
- **AND** the audit log still contains a `protected-file-watch` event for it
- **AND** `GET /sandbox/denied` still reports the path as protected with the
  mid-session note

### Requirement: Intuitive dialog order and safe default

The dialog SHALL put the safe choice first and order the remaining choices
from narrow to broad.

#### Scenario: Order

- **WHEN** the dialog is shown
- **THEN** the first option is `Keep warning every time (default)`
- **AND** the next options are one exact path each
- **AND** the batch option (when N > 1) follows the exact paths
- **AND** the folder options, when offered, are last
- **AND** Enter or cancel keeps every warning

### Requirement: No dialog backend

When no desktop dialog backend is available, omac SHALL fall back to the
existing informational popup and notification and SHALL keep the warnings.

#### Scenario: Headless launch

- **WHEN** neither osascript, zenity, nor kdialog is available
- **THEN** the user sees the informational batch notice
- **AND** the session-end summary lists the kept warnings
