# AGENTS.md

## E2E testing via Docker

The omac e2e suite (`internal/e2e/`, build tag `e2e`) verifies every
harness (opencode, claude-code, codex, copilot) can start under the omac
sandbox and call a skill through the facade. It runs on Linux (bwrap)
and macOS (nono). For local iteration on a host without the full
toolchain, use the Docker wrapper.

### Quick start

```sh
# Build the container (one-time; ~3 min on first build)
scripts/e2e-docker.sh build

# Run the echo-rest lifecycle test for one harness
SKAINET_TOKEN=... SKAINET_INTERNAL=... \
  scripts/e2e-docker.sh run opencode

# Run the security audit test
SKAINET_TOKEN=... SKAINET_INTERNAL=... \
  scripts/e2e-docker.sh audit opencode

# Run with a custom prompt (overrides the default echo-rest prompt)
SKAINET_TOKEN=... SKAINET_INTERNAL=... \
  scripts/e2e-docker.sh prompt "Check the echo-rest /echo endpoint with JSON"

# Drop into a shell inside the container
scripts/e2e-docker.sh shell

# Fetch test artifacts (stdout/stderr/meta.txt/sandbox profile)
scripts/e2e-docker.sh artifact opencode-linux-echo-rest | tar -x

# Stop the container
scripts/e2e-docker.sh stop
```

### What the script does

- `build` — builds `Dockerfile.e2e` (Ubuntu 24.04 + Go + bun + node +
  bubblewrap + AppArmor profile), starts a privileged container with
  the repo bind-mounted at `/repo`.
- `run` / `audit` / `prompt` — `docker exec` into the running container,
  injects `SKAINET_TOKEN` / `SKAINET_INTERNAL` / `ANTHROPIC_BASE_URL` as
  env vars, runs `go test -tags=e2e -v`.
- `logs` / `artifact` / `shell` / `stop` — container management.

### Platform notes

- **Linux hosts** (Fedora, Ubuntu, etc.): works with Docker or Podman
  (set `DOCKER_CMD=podman`). The container runs `--privileged` so bwrap
  can create user namespaces inside.
- **macOS hosts**: Docker Desktop provides a Linux VM; the same script
  works unchanged. `--privileged` is honored by Docker Desktop's VM.
  Apple-Silicon hosts use `linux/amd64` emulation (slower but works);
  set `--platform=linux/amd64` if the build picks the wrong arch.
- **No macOS containers exist** — Docker only runs Linux containers.
  macOS-specific code paths (nono/Seatbelt sandbox) are covered by the
  `e2e.yml` GitHub Actions matrix on `macos-latest`. Local Docker
  iteration covers the Linux (bwrap) path only.

### E2E_PROMPT env var

The `run` and `prompt` subcommands set `E2E_PROMPT` inside the container.
The test reads it (when wired — see `internal/e2e/e2e_test.go:runAgent`)
and substitutes the prompt. This lets an agent iterate on prompts
without editing the test source.

### Agent-driven workflow

An agent on the host can drive the e2e container via `bash`:

1. `scripts/e2e-docker.sh run opencode` — run the test.
2. Read `scripts/e2e-docker.sh artifact opencode-linux-echo-rest` —
   get stdout/stderr/meta.txt/sandbox profile.
3. `scripts/e2e-docker.sh prompt "new prompt"` — re-run with a variant.
4. Inspect failures via `scripts/e2e-docker.sh logs` or `shell`.

No MCP server needed — `bash` + `docker exec` + reading artifact files
is the full interface. If a restricted subagent (no bash) later needs to
drive the container, wrap these commands in an MCP server then; the
script remains the single source of truth.

## E2E from inside an omac sandbox (agent-driven local iteration)

`scripts/e2e-local.sh` lets an agent running inside an omac sandbox
(OMAC_SOCKET set) run the e2e suite without a host shell. Three blockers
that make the host-shell path fail inside a sandbox are auto-detected and
accommodated — CI runs are unaffected because it never sets OMAC_SOCKET.

```sh
# Smoke tier (no secrets, ~10s per harness) — install + CLI contract + launch probe
scripts/e2e-local.sh smoke opencode
scripts/e2e-local.sh smoke claude-code

# Full echo-rest lifecycle (needs SKAINET_TOKEN + SKAINET_INTERNAL)
SKAINET_TOKEN=... SKAINET_INTERNAL=... scripts/e2e-local.sh echo opencode

# Security audit (needs SKAINET_TOKEN + SKAINET_INTERNAL)
SKAINET_TOKEN=... SKAINET_INTERNAL=... scripts/e2e-local.sh audit opencode

# Default (no args) = smoke opencode
scripts/e2e-local.sh

# Pass extra go-test flags after --
scripts/e2e-local.sh smoke opencode -- -run TestHarnessCLIContract
```

### What the script auto-detects and fixes

When `OMAC_SOCKET` is set (only true inside an omac sandbox), the wrapper
exports three env vars the test code reads:

- `E2E_NESTED=1` — `omac start` runs with `--no-sandbox`. macOS denies
  nested Seatbelt profile application (`sandbox_apply: Operation not
  permitted`); the inner sandbox can't be applied from inside an existing
  one. The security audit's `sandboxActive` is derived from the same
  `forceNoSandbox` decision, so a nested run takes the "document exposure"
  branch (codex-on-macOS path) rather than asserting sandbox-active
  behavior against an unsandboxed agent.
- `E2E_RECOVER_INSTALL=1` — `installHarness` retries a failed install with
  `--ignore-scripts`, then runs the package's own `postinstall` script via
  `sh -c` directly. The omac sandbox blocks the postinstall subprocess the
  package manager spawns (EPERM on fork), leaving the binary without its
  platform binary. A direct `sh -c "$postinstall"` invocation works because
  it's not a spawned lifecycle hook. Also falls back from bun to npm for
  opencode when bun isn't on PATH (bun installs to `~/.bun`, which the
  sandbox blocks writes to).
- `TMPDIR=/tmp/omac-e2e` (wrapper) / `/tmp/omac-e2e-<pid>` (test code, for
  omac start/serve subprocesses) — short paths so the facade's
  `bridge.sock` stays under macOS's 104-byte `SUN_LEN` limit. The sandbox
  TMPDIR is a deep `/var/folders/...` path that makes
  `$TMPDIR/omac-<hash>/bridge.sock` exceed it, yielding
  `bind: invalid argument`.

### One-time host setup (outside the sandbox)

None required for the smoke tier. The wrapper detects missing bun and the
test code falls back to npm. If you want the faster bun install path, a
host-level `brew install bun` (outside the sandbox) is picked up
automatically.

For the full echo-rest / security-audit tiers you need secrets on the env:
`SKAINET_TOKEN`, `SKAINET_INTERNAL` (and `ANTHROPIC_BASE_URL` for
claude-code, which the wrapper passes through).

### Tiers

| Tier | Needs | What it does |
|------|-------|--------------|
| `smoke` | nothing | installs harness, checks `--help` flags omac depends on, runs `omac start -- --version` through the (no-sandbox) launch path. Under nesting the install runs the recovery path (full `--ignore-scripts` + manual postinstall) because the omac sandbox blocks the package manager's postinstall subprocess, so each harness install takes ~5-15s instead of <1s. |
| `echo` | secrets | full lifecycle: build omac, install harness, start agent, call echo-rest `/status`, assert `{"ok":true}` + workdir write + git commit |
| `audit` | secrets | security audit: inject a secret, run the self-audit probes, assert isolation properties (or document exposure under `--no-sandbox`) |

### Outside a sandbox

`scripts/e2e-local.sh` is a thin passthrough when `OMAC_SOCKET` is unset —
it sets none of the `E2E_NESTED`/`E2E_RECOVER_INSTALL` vars and just runs
`go test`. Use it on a host shell too; the bare `go test -tags=e2e ...`
recipe in `internal/e2e/e2e_test.go`'s doc comment also works.
