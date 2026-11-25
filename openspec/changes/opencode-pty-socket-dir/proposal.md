# Give each OpenCode session its own PTY socket folder

Issue: #520

## Why

Since OpenCode 2.3.0, a shell tool call inside an omac session on macOS can
take about 20 seconds, even for `echo hi`. The agent is then too slow to use,
and nothing in omac's logs says why.

How bad it is depends on the sandbox profile. Measured in a macOS 27 test VM
with OpenCode 2.3.2, per `echo hi` call after the first:

| Setup | Time per call | Helper works |
|---|---|---|
| OpenCode without omac | 0.02 s | yes |
| omac, fresh default profile | 0.2 s | no |
| omac, profile that makes `/tmp` writable (as in the report) | 20 s | no |
| omac, session with its own socket folder (this change) | 0.02 s | yes |

With the default profile the helper cannot create its socket and OpenCode
gives up quickly. With a writable `/tmp` the helper starts, the connection to
it is denied, and OpenCode waits.

The cause: OpenCode 2.3.0 runs shell commands through a helper process,
`opencode-pty`, which it reaches over a Unix socket in
`/tmp/opencode-pty-<uid>/`. The macOS sandbox (Seatbelt) treats a connection to
a Unix socket as a network operation, and omac's generated profile allows it
only for sockets it was told about. The connection is denied with EPERM.
OpenCode then retries the helper four times at 5 second intervals before it
runs the command directly. This repeats on every call.

| Period (from the reporter's `opencode.db`) | Median shell tool call |
|---|---|
| before OpenCode 2.3.0 | 0.04–0.18 s |
| OpenCode 2.3.0 and later | 20–40 s |

The profile field `filesystem.allow_unix_dir` already lifts the denial for one
folder. The issue proposes to grant `/tmp/opencode-pty-<uid>` with it by
default. That folder is shared by every OpenCode process of the user, and
granting it would open a way out of the sandbox:

- The helper accepts any client that presents a token. An OpenCode that runs
  outside omac stores the token in
  `~/.local/state/opencode/pty/<id>/service.json`. (A sandboxed OpenCode 2.x
  started with `omac start` keeps its own state in the session temp folder
  instead.)
- omac grants `~/.local/state/opencode` to every OpenCode session, because
  OpenCode keeps its state there. A sandboxed agent can therefore read the
  tokens of all helpers of that user.
- Today the denied connection is the only thing that stops the agent from
  using those tokens. With the shared folder granted, an agent could connect
  to the helper of an OpenCode that runs outside the sandbox, or in a session
  for another project, and ask it to run a command there.

Example: a prompt-injected agent in a sandboxed session reads the token of the
user's unsandboxed OpenCode, connects to its socket, and runs
`cat ~/.ssh/id_ed25519` outside the sandbox.

This attack was derived from OpenCode's documentation of the helper
(`packages/process-daemon/README.md` and `SPEC.md` in `anomalyco/opencode`)
and was not executed.

OpenCode offers a way around the shared folder. With the environment variable
`OPENCODE_PTY_SOCKET_DIR=<dir>`, the helper creates its socket in
`<dir>/opencode-pty-<uid>/` instead. The variable has existed since
2026-10-27, before 2.3.0, so every OpenCode version that has the stall also
supports it.

## What Changes

**1. A socket folder per session.** When omac launches the OpenCode harness in
the built-in sandbox, it:

- creates a folder `/tmp/omac-pty-<random>` that belongs to this session only,
- sets `OPENCODE_PTY_SOCKET_DIR` to that folder inside the sandbox,
- grants the session read, write and Unix-socket connect on that folder, the
  same grant that `--allow-unix-dir` gives,
- removes the folder when the session ends.

The user configures nothing.

**2. The shared folder becomes a protected path.** `/tmp/opencode-pty-<uid>`
joins omac's built-in list of protected paths on macOS and Linux, next to the
Docker socket. A protected path stays closed even when a profile grants it,
and `omac doctor` reports such a grant with high severity. This applies to
every harness, not only OpenCode.

Some users have applied the workaround from #520 and granted the shared folder
in their profile. With this change that grant stops working and is reported.
Their sessions keep working, because OpenCode now uses the per-session folder.

| Session | Before | After |
|---|---|---|
| `omac start opencode`, OpenCode ≥ 2.3.0, macOS | every shell call stalls ~20 s | shell calls run through the session's own helper, no stall |
| `omac serve opencode`, OpenCode ≥ 2.3.0, macOS | same stall | same fix |
| OpenCode < 2.3.0 | no stall | no stall; the variable is set but unused |
| Sandboxed agent connects to another OpenCode's helper | denied | still denied |
| Profile with `allow_unix_dir: ["/private/tmp/opencode-pty-501"]` | connect to all helpers of the user allowed | denied, and reported by `omac doctor` |
| `omac start claude` and other harnesses | not affected | no folder is created; the shared folder is protected |

## Decisions

All decisions below were made with the maintainer.

- **A folder per session, not the shared folder.** See "Why".
- **The folder lives directly under `/tmp`.** macOS limits a Unix socket path
  to 103 bytes. The helper appends `/opencode-pty-<uid>/<32 hex chars>.sock`,
  which is up to 62 bytes for a 10-digit uid. omac's existing session temp
  folder (`$TMPDIR/omac-sandbox-tmp-<n>`, 76 bytes on a typical Mac) is
  therefore too long:

  | Base folder | Full socket path | Fits |
  |---|---|---|
  | `/var/folders/…/T/omac-sandbox-tmp-3901748501` | 131 bytes | no |
  | `/tmp/omac-pty-3901748501` | 79 bytes | yes |

  The kernel sees `/tmp` as `/private/tmp`, 8 bytes longer. That still fits,
  and the rule generator already writes both forms of a path.
- **Built-in sandbox only.** With `--no-sandbox` nothing is denied, and
  OpenCode's default folder works. With the deprecated nono backend, omac
  cannot grant the folder, so setting the variable would move the helper to a
  place the user's nono profile does not allow. In both cases omac creates no
  folder and sets no variable. nono users fix the stall in their nono profile.
- **Same behavior on Linux.** Linux does not filter Unix-socket connections,
  so there is no stall today. omac applies the same folder and the same
  protected path there, to keep one code path that the Linux CI exercises.
- **All OpenCode versions get the variable.** omac does not check the OpenCode
  version. Versions before 2.3.0 do not use the helper for shell commands, so
  the variable has no effect there. A version check could fail silently and
  bring the stall back.
- **omac replaces a value from the host.** If the user has exported
  `OPENCODE_PTY_SOCKET_DIR`, the session still gets omac's folder, without a
  message. The host value points at a folder the session cannot connect to.
- **A profile that blocks the variable stops the launch.** If the user's
  profile lists the variable in `environment.deny_vars`, for example through
  `OPENCODE_*`, omac refuses to start with an error that names the variable.
  Continuing would bring the stall back without a hint. The variable joins the
  existing check `openCodePinDenied`
  (`internal/cli/opencode_v2_service.go`), which already stops such profiles
  for OpenCode 2.x.
- **No cleanup of leftovers after a crash.** If omac is killed, the folder
  stays until `/tmp` is emptied. It holds only socket files. The existing
  session temp folder has the same gap; a sweep would have to cover both and
  is not part of this change.
- **No change to the rule generators.** `--allow-unix-dir` writes only a
  connect rule. The helper may listen in the folder because every built-in
  launch passes `--open-port` for omac's own port, and one port grant
  currently allows listening everywhere on macOS. The change
  `macos-listen-all-ports` (#521) replaces that with a rule for all paths the
  session may write to, which includes the new folder. This change works
  before and after #521.
- **The protected path relies on #512.** On today's `main`, a protected path
  on macOS blocks file access but not a socket connect. Pull request #513
  ("close protected paths to socket use", closes #512) fixes that in the rule
  generator. This change does not repeat that fix. Its pull request is rebased
  after #513 is merged. If #513 is delayed for long, part 2 is split into its
  own pull request so that the stall fix can ship.
- **One pull request** for both parts, the tests and the docs.
- **Not a profile setting.** The folder is created per launch, so it cannot be
  written into a profile file. It is added at launch, the same way the tool
  cache folder is (`injectSandboxFlag(argv, "--allow", …)` in
  `internal/cli/start.go`). Users who copied a launcher template get the fix
  without editing it.

## Tradeoffs

Each row names an alternative that was considered and rejected, and what the
chosen design costs.

| Choice | Rejected alternative | Why rejected | What the choice costs |
|---|---|---|---|
| Per-session socket folder | Grant the shared `/tmp/opencode-pty-<uid>` by default, as #520 suggests and as nono did (nolabs-ai/nono-packs#69) | One profile line, but it lets a sandboxed agent reach the helpers of unsandboxed OpenCode processes and of other sessions, whose tokens it can read. That is a way out of the sandbox | New launch code in two commands, a second temp folder per session, and a dependency on an OpenCode variable that omac does not control |
| Shared folder on the protected list | Leave it ungranted but grantable; or only report it in the audit | Users who copied the #520 workaround would keep the escape path open without noticing. A report alone does not close it | An existing profile grant silently stops working. Lifting it needs `override_deny`. On macOS it only holds once #513 is merged |
| Refuse to launch when `deny_vars` blocks the variable | Start anyway, with or without a warning | The stall is silent and looks like a broken agent; a launch warning scrolls away | A profile with a broad `OPENCODE_*` deny no longer starts OpenCode 1.x either, where the variable is not needed |
| Same folder and protected path on Linux | macOS only | Two code paths, and the fix would run only on the platform with the weakest automated coverage | A change on a platform where nothing stalls today, and one more folder under `/tmp` per session |
| No OpenCode version check | Set the variable only for 2.x | A wrong version detection would bring the stall back silently | OpenCode 1.x sessions get an unused variable and an empty folder |
| Built-in sandbox only | Set the variable for the nono backend too | omac cannot grant the folder to nono, so the helper would move out of reach of the user's nono profile | nono users on OpenCode ≥ 2.3.0 keep the stall until they change their nono profile |
| Host state folder stays readable | Hide `~/.local/state/opencode` in this change | Safe only where omac redirects OpenCode's state, which today is `omac start` with OpenCode 2.x. Elsewhere it would break the helper and bring the stall back | Helper tokens and prompt history of other OpenCode processes stay readable. The tokens are unusable, because the sockets are blocked twice |
| Reuse #513 for the generator gap | Fix protected-path socket connects here | It would duplicate an open pull request and conflict with it | The protected-path part waits for #513 |
| One pull request | Split the stall fix from the protected path | Decided by the maintainer: fix and hardening ship together | The stall fix is tied to the merge of #513, unless the split in `tasks.md` is used |
| No sweep of leftover folders | Remove stale `/tmp/omac-pty-*` at launch | A wrong liveness check would delete a running session's folder | Empty folders remain after a crash until `/tmp` is emptied |

## Capabilities

### Modified Capabilities

- `sandbox-launch`: one added requirement for the per-session PTY socket
  folder.
- `sandbox-process-isolation`: one added requirement that the shared PTY
  folder is a protected path.

`openspec/specs/` holds no baseline for these capabilities yet. Their
requirements live in the active change `native-sandbox`, so `native-sandbox`
must be archived before this change is archived.

## Impact

- **`internal/config/harness.go`**: the OpenCode harness declares that it needs
  a socket folder and under which variable. `OPENCODE_PTY_SOCKET_DIR` joins its
  `SandboxEnvAllow`, the list of variables omac forwards into the sandbox. The
  existing hook `LaunchEnvFunc(configHome, tmpDir)` cannot be reused as is,
  because it only knows the long temp folder.
- **`internal/cli/start.go` and `internal/cli/serve.go`**: create the folder
  next to the existing session temp folder, remove it on exit, pass
  `--allow-unix-dir <folder>` to `omac sandbox run`, and export the variable.
  Both files do these steps separately today; the new code should be shared.
- **`internal/cli/opencode_v2_service.go`**: the `deny_vars` check covers the
  new variable. Today the check runs only for OpenCode 2.x; it must run for
  every OpenCode launch that sets the variable.
- **`internal/sandboxprofile/baseline.go`**: the protected-path lists for
  macOS and Linux gain `/tmp/opencode-pty-<uid>`, computed from the current
  uid. The profile audit (`internal/profileaudit`) reports grants that cover a
  protected path already; no new audit rule is needed.
- **Tests**: see `tasks.md`. This change adds the first end-to-end test that
  runs a real OpenCode 2.x.
- **Docs**: two rows in the access table of `docs/security.md` and one
  sentence in `docs/advanced/nono.md`. No troubleshooting entry, because a
  user on a current omac has nothing to do.

## Risks

- **Not yet run with a real OpenCode.** The design rests on OpenCode's source
  and on omac's existing sandbox test for `allow_unix_dir`
  (`TestIntegrationAllowUnixDirGrantsSocketAccess`). The first task is a manual
  proof on macOS before any code is written.
- **The new end-to-end test may not fit this change.** The suite pins OpenCode
  1.18.29, the oldest supported version, and has never installed a real 2.x.
  If 2.x cannot be installed through the suite's existing route, the test
  becomes a follow-up issue and the manual check in `tasks.md` stands in.
  Without that test nothing notices if OpenCode renames the variable or moves
  the folder.
- **Linux hides a protected folder only if it exists at launch.** If a profile
  grants the real `/tmp` and an unsandboxed OpenCode starts later, its socket
  folder appears unhidden. This is how Linux protection works for every
  protected path; `docs/security.md` states it for this one.
- **Users of the #520 workaround see a new `omac doctor` finding.** The finding
  is correct and their sessions keep working, but it is a visible change that
  belongs in the release notes.

## Out of Scope

Follow-up issues:

- Making denied Unix-socket connections visible in `omac diagnose` or
  `omac doctor`. Seatbelt denies them in the kernel, so omac sees nothing
  today. (From #520.)
- User documentation for `filesystem.allow_unix_dir`. (From #520; related to
  #416.)
- Hiding the host's OpenCode state folder `~/.local/state/opencode` from
  sessions whose state omac redirects. Other sessions' helper tokens and the
  host's prompt history stay readable after this change. The tokens cannot be
  used, because the sockets stay blocked.

Also out of scope:

- The rule-generator fix for protected paths (#512, pull request #513).
- The fix on OpenCode's side that remembers a failed helper start instead of
  retrying on every call (`anomalyco/opencode#64080`).
- The stall under the deprecated nono backend.
