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

#### Scenario: Ignore one file

- **WHEN** the dialog lists `/repo/.worktrees/a/.env.local` and the user picks
  `Ignore this file permanently`
- **THEN** that exact path is stored under `protected_watch_ignore`
- **AND** the same path is not reported again in this or a later session
- **AND** the file remains protected at launch and is still audit-logged

#### Scenario: Exact path does not cover a sibling

- **WHEN** the user ignored `/repo/.worktrees/a/.env.local`
- **AND** `/repo/.worktrees/b/.env.local` is created later
- **THEN** the second path is still reported

### Requirement: Batch exception

The dialog SHALL let the user dismiss every file listed in the current batch in
one choice.

#### Scenario: Ignore the current batch

- **WHEN** the dialog lists three paths and the user picks
  `Ignore all 3 files permanently`
- **THEN** all three exact paths are stored in `protected_watch_ignore`
- **AND** none of them is reported again in this session
- **AND** a fourth path created later is still reported

### Requirement: Project-scoped name exception

The dialog SHALL offer a project-scoped basename exception when the session
workdir resolves to a git root that contains the detected path.

#### Scenario: New worktree of the same repository

- **WHEN** the user picks `Ignore every .env.local in this project (yarp3)`
  for `/repo/.worktrees/a/common/.env.local`
- **AND** the resolved git root is `/repo`
- **THEN** `{root: /repo, name: .env.local}` is stored under
  `protected_watch_project_ignores`
- **AND** a later `/repo/.worktrees/b/common/.env.local` is not reported
- **AND** `/repo/.env.local` is not reported

#### Scenario: Another repository keeps the warning

- **WHEN** the user stored `{root: /repo, name: .env.local}`
- **AND** `/other-repo/.env.local` is created
- **THEN** `/other-repo/.env.local` is still reported

#### Scenario: Root boundary is exact

- **WHEN** the stored root is `/repo`
- **AND** a path is `/repo-other/.env.local`
- **THEN** the project exception does not match

#### Scenario: No project option without a git root

- **WHEN** the workdir is not inside a git repository, or git fails, times
  out, or returns a root that does not contain the workdir
- **THEN** the dialog does not offer a project-scoped option
- **AND** the exact-path and batch options remain

#### Scenario: Project choice does not hide other names

- **WHEN** the batch contains `.env.local` and `.env` under the same root
- **AND** the user picks the `.env.local` project option
- **THEN** only the `.env.local` paths under that root are dismissed
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
- **AND** it cannot create a project-scoped exception without the user's
  dialog choice

### Requirement: Protection and audit are unchanged

An exception SHALL suppress only the notice. The path SHALL remain in the
facade's protected set, SHALL remain masked at launch when it exists then, and
SHALL still produce one audit event per detection.

#### Scenario: Ignored file is still audited and readable

- **WHEN** a path matches the project exception
- **THEN** no dialog is shown for it
- **AND** the audit log still contains a `protected-file-watch` event for it
- **AND** `GET /sandbox/denied` still reports the path as protected with the
  mid-session note

### Requirement: Intuitive dialog order and safe default

The dialog SHALL put the safe choice first and order the remaining choices
from narrow to broad.

#### Scenario: Order

- **WHEN** the dialog is shown
- **THEN** the first option is `Keep warning for N files (report at exit)`
- **AND** the next options are one exact path each
- **AND** the batch option (when N > 1) follows the exact paths
- **AND** the project option, when offered, is last
- **AND** Enter or cancel keeps every warning

### Requirement: No dialog backend

When no desktop dialog backend is available, omac SHALL fall back to the
existing informational popup and notification and SHALL keep the warnings.

#### Scenario: Headless launch

- **WHEN** neither osascript, zenity, nor kdialog is available
- **THEN** the user sees the informational batch notice
- **AND** the session-end summary lists the kept warnings
