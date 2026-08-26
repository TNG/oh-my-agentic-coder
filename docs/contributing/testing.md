---
title: Testing
description: omac's testing strategy — what is automated, what requires manual verification, and how to run each tier.
---

## Overview

omac has three tiers of automated tests and a set of areas that require manual verification.

---

## Unit and integration tests

These run on every PR via CI. They need no harness installation or provider credentials.

```bash
go test ./...          # all packages
go test -race ./...    # with race detector (matches CI)
```

These cover: CLI subcommands, config parsing, sandbox grant resolution, the HTTP facade (SSE and WebSocket forwarding), the network proxy, keychain reads and writes, skill registry atomic writes, and plugin discovery.

Platform-specific behaviour lives in separate files gated by a `//go:build` constraint — `*_integration_linux_test.go` (tagged `linux`) tests bwrap and Landlock; `*_integration_darwin_test.go` (tagged `darwin`) tests Seatbelt. Go compiles each file only on its target OS, so on the other OS those tests are left out of the test binary entirely.

Runtime skipping is a separate mechanism: some facade and serve tests are compiled everywhere but skip themselves at runtime when they cannot open a loopback TCP port or Unix socket (e.g. on locked-down CI runners), and these do show up as skipped in the test output.

---

## E2E tests

E2E tests run against real installed harnesses and are **not** run on every PR. Running them requires real model credentials and takes several minutes; the full suite runs weekly on `main` instead (and can be triggered manually on any branch). Run them locally when your change touches the sandbox, harness integration, or the Facade.

```bash
go test -tags=e2e -v ./internal/e2e/
```

To run a single stage in isolation:

```bash
# Only the contract check (no credentials needed):
go test -tags=e2e -run TestHarnessCLIContract -v ./internal/e2e/

# Only sandbox_denied:
go test -tags=e2e -run TestE2ESandboxDenied -v ./internal/e2e/
```

E2E tests use temporary home directories and do not modify your real keychain or global omac config.

| Stage | What it tests | Needs credentials? | Harnesses             |
|---|---|---|-----------------------|
| `contract` | Every CLI flag omac depends on is still present in the harness's `--help` | no | all                   |
| `launch` | omac boots the real harness inside the sandbox — checked by running the harness's own `--version`, so no model call | no | all                   |
| `serve` | `omac serve` boots the harness's HTTP daemon in the sandbox, then opens a project directory, reads its skills manifest, and closes it | no | OpenCode Desktop only |
| `llm` | A single agent turn through the [echo-rest](echo-rest.md) skill verifies model auth and the sidecar facade | yes | all                   |
| `cache_isolation` | Tool cache scoping — downloaded packages are redirected to an isolated directory, not `~/.cache` | no | all                   |
| `sandbox_denied` | Protected paths (`.ssh`, `.aws`, `.env`, …) stay inaccessible inside the sandbox | no | all                   |
| `security_assertions` | A live agent runs an in-sandbox self-audit script; the test then checks its output to confirm protected files (read/write, incl. symlink escape), secrets, filtered env vars, and disallowed network requests were all blocked, while allowed paths still worked | yes | all                   |
| `self_authored_skill` | A user-written skill can be registered and executed | no | all                   |

**For the `llm` stage:** credentials are read from environment variables at runtime — `internal/e2e/versions.go` declares which variable names, and `.github/workflows/e2e.yml` wires them from GitHub Actions encrypted secrets. When running tests locally, you supply credentials through your own shell environment.
All harnesses except claude-code use a shared model gateway (`zai-org/GLM-5.2`) because it is cost-effective.
claude-code is the exception because it communicates directly with the Anthropic API — it cannot route through a third-party gateway — so it uses `claude-sonnet-5` with an Anthropic key.

---

## Harness compatibility drift detection

The weekly `E2E: drift` workflow installs the **latest released version** of every harness (not a pinned version) and runs the model-free stages (those needing no credentials — see the `Needs credentials?` column above) against both `main` and the latest published omac binary.

The workflow exists because a harness release can silently break omac by renaming a CLI flag, moving a subcommand, or changing a config schema. At runtime, omac simply trusts its own harness registry (`internal/config/harness.go`). The `contract` test is the safety net: it compares that registry against the installed harness's actual `--help` (`internal/e2e/harnesses.go`), so a mismatch surfaces in CI before it reaches users.

### Running the model-free checks locally

```bash
go test -tags=e2e -run 'TestHarnessCLIContract|TestHarnessLaunchProbe|TestHarnessServeProbe' -v ./internal/e2e/
```

The `serve` probe requires opencode to be installable via [bun](https://bun.sh) (a JavaScript runtime used by opencode) and a working sandbox backend — bubblewrap on Linux or Seatbelt on macOS.

### What to do when the drift suite fails

- **`contract` failure**: the harness renamed or removed a flag omac depends on. Fix the harness descriptor in `internal/config/harness.go` and the e2e contract in `internal/e2e/harnesses.go`. `internal/config/harness.go` declares what CLI flags omac uses; `internal/e2e/harnesses.go` declares which of those to validate against the harness's `--help`.
- **`launch` failure**: usually a sandbox grant or PATH issue. Run `omac doctor` to check whether the harness binary is on `PATH` and the sandbox backend is working. Check `~/.local/state/omac/sandbox.log` (Linux) or `~/Library/Logs/omac/sandbox.log` (macOS) for denied paths.
- **`llm` failure**: often a model availability or auth issue, not an omac regression.

### Where results live

- **[GitHub tracking issue](https://github.com/TNG/oh-my-agentic-coder/issues?q=is%3Aissue+label%3A%22auto-update+tracker%22)**: titled *"Harness compatibility matrix"*, label `auto-update tracker`. Updated in place each run with a rolling 30-day history. This is the public place to check when CI turns red.
- **Private archive repo**: maintainer-only; full diffable history of every run.
- **Slack**: maintainer-only; pass/fail summary posted after each run.

---

## Manual testing

These areas have no automated coverage and must be verified by hand, especially before a release:

- **Installation packages**: Homebrew tap trust and `brew install`, Debian `.deb` postinstall, Arch `.pkg.tar.zst` via pacman.
- **Network prompt dialog**: zenity/kdialog (Linux) and osascript (macOS) dialogs require a real display session. CI runs them with `xvfb-run` on a best-effort basis; visual appearance and button behaviour need a human.
- **Keychain interaction**: macOS Keychain unlock prompts and Linux Secret Service interaction (especially the WSL2 setup flow) require a live user session.
- **WSL2 end-to-end**: The full WSL2 keychain setup (dbus, gnome-keyring, Secret Service) needs a real WSL2 instance to verify — neither WSL detection (`internal/osinfo/`) nor the live keychain wiring is covered by automated tests.
- **Platform sandbox binaries**: Confirming bwrap + Landlock on a given kernel version, or Seatbelt on a given macOS release, requires running on that platform.
