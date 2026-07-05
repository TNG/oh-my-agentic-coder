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
