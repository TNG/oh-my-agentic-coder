# profileaudit

## Overview

`profileaudit` lints a resolved sandbox profile for risky settings, such as a
grant that exposes `~/.ssh` or an allowed cloud-metadata host, and returns
findings sorted by severity. It powers `omac provenance --check` and does no
filesystem or network I/O.

## Interface

- `Check(profile *sandboxprofile.Profile) []Finding`: runs override_deny,
  filesystem, network and environment checks; sorted by severity, category,
  field, value.
- `Finding{Severity, Category, Field, Value, Message}` (JSON-tagged).
- `Severity` (`high`, `medium`, `low`), `Category` (`filesystem`, `network`,
  `override_deny`, `environment`).
- `ExitCode(findings) int`: 2 if any HIGH finding, else 0.
- `BaselineSecretPaths()`: the platform baseline's protected paths.
- `ExtensionSecretPaths`: extra secret paths (`~/.pypirc`, `~/.config/gh`,
  `~/.gitconfig`, ...).
- `SecretBasenameGlobs`: `.env`, `*.env`, `*.key`, `*.pem`, `*token*`,
  `*secret*`, `id_rsa*`, `*.pfx`, `*.p12`.

## Invariants

- An `override_deny` entry that removes a baseline-protected path is HIGH.
- A grant equal to or above a secret path is flagged; a grant below it is not.
- Broad grants (`*`, `.`, `./`) give one MEDIUM finding per secret glob.
- Cloud-metadata hosts in `allow_domain` are HIGH; `*.internal`/`*.local` are
  MEDIUM; every `open_port` is LOW; TCP connect to 22 or 3389 is MEDIUM; empty
  `allow_vars` is MEDIUM.
- Only HIGH affects the exit code.
- `ExtensionSecretPaths` must not overlap the baseline; globs must be valid
  `filepath.Match` patterns (guard tests).
- Must not import `internal/cli` (import cycle).

## Side Effects

- None, apart from reading the home dir and env for path expansion.

## Dependencies

[`../sandboxprofile`](../sandboxprofile/EGM.md).

## Verification

- Run: `go test ./internal/profileaudit/...`
