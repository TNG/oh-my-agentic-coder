# Tasks: opencode-pty-socket-dir

One pull request for all sections. Sections 1 to 3 remove the stall. Section 4
builds on pull request #513 (merged 2026-11-26), which makes protected paths
block socket connects on macOS.

## 1. Prove the approach by hand first (macOS, OpenCode ≥ 2.3.0)

No code change. Add the folder to the active sandbox profile
(`~/.config/omac/sandbox-profiles/default.json`) and export the variable:

```json
"filesystem":  { "allow_unix_dir": ["/tmp/omac-pty-proof"] },
"environment": { "allow_vars": ["OPENCODE_PTY_SOCKET_DIR"] }
```

```bash
mkdir /tmp/omac-pty-proof
env OPENCODE_PTY_SOCKET_DIR=/tmp/omac-pty-proof omac start opencode
```

The `allow_vars` entry is required: omac forwards only listed variables into
the sandbox. Any setup with the same effect is fine.

- [ ] 1.1 A shell tool call returns in well under a second, and OpenCode's log
  (`~/.local/share/opencode/log/opencode.log`) has no
  `process daemon unavailable` line for it
- [ ] 1.2 A socket exists under `/tmp/omac-pty-proof/opencode-pty-<uid>/`
  while the session runs
- [ ] 1.3 Inside the session, the probe from issue #520 against
  `/tmp/opencode-pty-<uid>` still fails with EPERM on connect
- [ ] 1.4 Record the OpenCode and `opencode-pty` versions used. If 1.1 fails,
  stop and return to the proposal

## 2. Harness declaration (`internal/config/harness.go`)

- [ ] 2.1 The OpenCode harness declares the variable name for its socket
  folder. Other harnesses declare nothing
- [ ] 2.2 `OPENCODE_PTY_SOCKET_DIR` is in the OpenCode harness's
  `SandboxEnvAllow`
- [ ] 2.3 Review the comments on `LaunchEnvFunc` and the OpenCode harness
  entry; keep them accurate

## 3. Launch (`internal/cli/start.go`, `internal/cli/serve.go`)

- [ ] 3.1 One shared helper creates the folder under `/tmp` with mode 0700 and
  returns a cleanup function. Used by both `start` and `serve`
- [ ] 3.2 Only when the launch uses the built-in sandbox and the harness
  declares a socket folder: inject `--allow-unix-dir <folder>` and export the
  variable. Follow how the tool cache folder is granted
  (`injectSandboxFlag(argv, "--allow", cacheScope.Dir)`). No OpenCode version
  check. omac's value replaces one inherited from the host
- [ ] 3.3 If the profile's `environment.deny_vars` matches the variable,
  refuse the launch with an error that names it. Extend `openCodePinDenied`
  (`internal/cli/opencode_v2_service.go`) or share its logic; the check must
  run for OpenCode 1.x launches and for `serve` too, not only when the
  OpenCode 2.x service pin applies
- [ ] 3.4 The folder is removed on every exit path that already removes the
  session temp folder

## 4. Protected path (`internal/sandboxprofile/baseline.go`)

- [ ] 4.1 Add `/tmp/opencode-pty-<uid>`, computed from the current uid, to the
  protected paths for macOS and Linux
- [ ] 4.2 Confirm that the profile audit reports a grant covering it with high
  severity without a new rule. If the audit's path list is not derived from
  the protected paths, add the entry there

## 5. Tests

Launch wiring:

- [ ] 5.1 Unit, path budget: for the folder name pattern and a 10-digit uid,
  `/private/tmp/<name>/opencode-pty-<uid>/<32 hex>.sock` is shorter than 104
  bytes. This guards any later rename of the folder
- [ ] 5.2 Unit, launch wiring: for the OpenCode harness with the built-in
  launcher, the sandbox command contains `--allow-unix-dir <folder>` and the
  environment contains `OPENCODE_PTY_SOCKET_DIR=<folder>`, with the same
  folder in both, also when the host environment sets another value. For
  `claude`, for `--no-sandbox` and for a nono launcher, neither is present.
  Cover `start` and `serve`
- [ ] 5.3 Unit: a profile with `deny_vars: ["OPENCODE_*"]` makes the launch
  fail with the error from 3.3, for OpenCode 1.x and 2.x
- [ ] 5.4 Integration, macOS, real sandbox (`internal/sandboxrun`): with the
  grants of a real launch (`--allow-unix-dir <A>` plus one `--open-port`), a
  sandboxed process can listen on and connect to a socket at
  `<A>/opencode-pty-<uid>/<32 hex>.sock`. The existing
  `TestIntegrationAllowUnixDirGrantsSocketAccess` covers connect only, with
  the listener outside the sandbox
- [ ] 5.5 The folder is gone after the launch returns

Protected path:

- [ ] 5.6 Unit: the resolved protected paths contain the folder for the
  current uid on both platforms, and `override_deny` removes it
- [ ] 5.7 Integration, macOS, real sandbox: with
  `allow_unix_dir: ["/tmp/opencode-pty-<uid>"]` in the profile, and again with
  `allow_unix_dir: ["/tmp"]`, a connect to a socket in that folder served by
  the test process fails with EPERM. Assert the errno. Use a test uid or a
  test-only folder name so the test does not touch a real helper
- [ ] 5.8 Unit, audit: a profile whose `allow_unix_dir` covers the folder
  yields a high-severity finding

End to end:

- [ ] 5.9 A model-free test with a real OpenCode ≥ 2.3.0 (`internal/e2e`):
  start `omac serve opencode` in the built-in sandbox, send `echo hi` through
  `POST /api/shell`, and fail if the call takes longer than a few seconds. It
  selects its own OpenCode version and leaves the 1.18.29 pin
  (`internal/e2e/pinned_package.go`) unchanged. Run it on macOS and Linux
- [ ] 5.10 Fallback for 5.9: if OpenCode 2.x cannot be installed through the
  suite's existing route, open a follow-up issue for the test, state the gap
  in the pull request, and rely on 6.1

## 6. Manual checks

- [ ] 6.1 macOS, `omac start opencode` with the latest OpenCode and a fresh
  default profile: time `echo hi` through the shell tool. Put the timing and
  versions into the pull request's verification section
- [ ] 6.2 macOS, `omac serve opencode` with OpenCode Desktop: same check
- [ ] 6.3 Linux, `omac start opencode`: a shell tool call works and the socket
  is created under the session's folder
- [ ] 6.4 Two sessions in parallel on macOS: each has its own folder, and
  ending one does not disturb shell calls in the other
- [ ] 6.5 macOS, profile with the #520 workaround grant: the session works,
  the probe against the shared folder fails, and `omac doctor` reports the
  grant

## 7. Docs

- [ ] 7.1 `docs/security.md`, access table: one row for the per-session PTY
  folder (read, write and connect, OpenCode harness only, with the reason)
- [ ] 7.2 `docs/security.md`, access table: one row for the shared
  `/tmp/opencode-pty-<uid>` (blocked, with the reason, `override_deny` as the
  way to lift it, and the Linux limit that the folder is hidden only if it
  exists at launch)
- [ ] 7.3 `docs/advanced/nono.md`: one sentence that the fix does not cover
  the nono backend, and that nono users grant the helper's folder in their
  nono profile
- [ ] 7.4 No troubleshooting entry

## 8. Issue housekeeping

- [ ] 8.1 Comment on #520 when the pull request is open: why the shared-folder
  workaround should not be used, and a link to the fix
- [ ] 8.2 Put a release-notes warning into the pull request: users who added
  the #520 workaround (`allow_unix_dir` for `/tmp/opencode-pty-<uid>`) should
  delete that grant. It no longer has an effect, omac warns about it at
  launch, and `omac doctor` reports it. They should not follow the warning's
  hint to add `override_deny`
- [ ] 8.3 After the implementation is done: the maintainer reviews the three
  follow-up issue drafts listed under "Out of Scope" in the proposal, and they
  are posted. They are drafted but not yet filed
