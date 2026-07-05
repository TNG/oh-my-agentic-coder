#!/usr/bin/env bash
#
# e2e-docker.sh — drive the omac e2e container from the host.
#
# Platform-agnostic: works the same on Linux, macOS, and WSL hosts.
# Requires Docker (or a Docker-compatible runtime via DOCKER_CMD).
#
# The container image (Dockerfile.e2e) bakes in: Go, bun, node, bubblewrap,
# and the AppArmor profile needed for bwrap userns. Build it once, then use
# this script to drive test runs, fetch artifacts, set custom prompts, or
# drop into a shell.
#
# Usage:
#   scripts/e2e-docker.sh build                  # build the e2e image
#   scripts/e2e-docker.sh run [harness] [prompt] # run TestE2EEchoRest
#   scripts/e2e-docker.sh audit [harness]        # run TestE2ESecurityAudit
#   scripts/e2e-docker.sh logs                   # tail container logs
#   scripts/e2e-docker.sh artifact <name>       # copy artifact dir to stdout
#   scripts/e2e-docker.sh prompt <text>          # run echo-rest with a custom prompt
#   scripts/e2e-docker.sh shell                  # interactive shell in the container
#   scripts/e2e-docker.sh stop                   # stop + remove the container
#
# Environment:
#   E2E_IMAGE       image name (default: omac-e2e)
#   E2E_CONTAINER   container name (default: omac-e2e)
#   E2E_LOG_DIR     artifact dir inside container (default: /tmp/e2e-logs)
#   SKAINET_TOKEN   model API key (required for run/audit/prompt)
#   SKAINET_INTERNAL  model provider base URL (required for run/audit/prompt)
#   ANTHROPIC_BASE_URL  Anthropic proxy URL (claude-code only)
#   DOCKER_CMD      docker runtime (default: docker; set to podman, etc.)
#
# Exit code 0 = success; non-zero = failure.

set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
IMAGE="${E2E_IMAGE:-omac-e2e}"
CONTAINER="${E2E_CONTAINER:-omac-e2e}"
LOG_DIR="${E2E_LOG_DIR:-/tmp/e2e-logs}"
DOCKER="${DOCKER_CMD:-docker}"

# Common env flags passed to every run/audit/prompt invocation.
# macOS hosts: Docker Desktop injects these via --env-file or --env; same flags work.
env_flags() {
    local -a flags=()
    [[ -n "${SKAINET_TOKEN:-}" ]]        && flags+=(-e "SKAINET_TOKEN=${SKAINET_TOKEN}")
    [[ -n "${SKAINET_INTERNAL:-}" ]]     && flags+=(-e "SKAINET_INTERNAL=${SKAINET_INTERNAL}")
    [[ -n "${ANTHROPIC_BASE_URL:-}" ]]   && flags+=(-e "ANTHROPIC_BASE_URL=${ANTHROPIC_BASE_URL}")
    printf '%s\n' "${flags[@]}"
}

# Ensure container is running; start it if needed.
ensure_running() {
    if ! "$DOCKER" ps --format '{{.Names}}' | grep -qx "$CONTAINER" 2>/dev/null; then
        if "$DOCKER" ps -a --format '{{.Names}}' | grep -qx "$CONTAINER" 2>/dev/null; then
            "$DOCKER" start "$CONTAINER" >/dev/null
        else
            echo "Container '$CONTAINER' not found. Run: $0 build" >&2
            exit 1
        fi
    fi
}

require_secret() {
    if [[ -z "${!1:-}" ]]; then
        echo "Missing env var: $1 (set it before running this command)" >&2
        exit 1
    fi
}

cmd_build() {
    echo "== building e2e image: $IMAGE =="
    "$DOCKER" build -t "$IMAGE" -f "$REPO/Dockerfile.e2e" "$REPO"
    echo "== starting container: $CONTAINER =="
    "$DOCKER" rm -f "$CONTAINER" 2>/dev/null || true
    # --privileged: bwrap needs userns + AppArmor unconfined inside the container.
    # On macOS Docker Desktop this works with the default VM; no extra config needed.
    "$DOCKER" run -d --name "$CONTAINER" \
        --privileged \
        -v "$REPO:/repo" \
        -w /repo \
        "$IMAGE" sleep infinity
    echo "== container ready. Run: $0 run opencode =="
}

cmd_run() {
    local harness="${1:-opencode}"
    local prompt="${2:-}"
    require_secret SKAINET_TOKEN
    require_secret SKAINET_INTERNAL
    ensure_running
    local -a env_args=()
    if [[ -n "$harness" ]]; then env_args+=(-e "E2E_HARNESS=$harness"); fi
    if [[ -n "$prompt" ]]; then env_args+=(-e "E2E_PROMPT=$prompt"); fi
    # shellcheck disable=SC2207
    local -a sec=($(env_flags))
    "$DOCKER" exec -i "${env_args[@]}" "${sec[@]}" \
        -e "E2E_LOG_DIR=$LOG_DIR" \
        "$CONTAINER" go test -tags=e2e -timeout=30m -v -run TestE2EEchoRest ./internal/e2e/
}

cmd_audit() {
    local harness="${1:-opencode}"
    require_secret SKAINET_TOKEN
    require_secret SKAINET_INTERNAL
    ensure_running
    local -a env_args=()
    if [[ -n "$harness" ]]; then env_args+=(-e "E2E_HARNESS=$harness"); fi
    # shellcheck disable=SC2207
    local -a sec=($(env_flags))
    "$DOCKER" exec -i "${env_args[@]}" "${sec[@]}" \
        -e "E2E_LOG_DIR=$LOG_DIR" \
        "$CONTAINER" go test -tags=e2e -timeout=30m -v -run TestE2ESecurityAudit ./internal/e2e/
}

cmd_logs() {
    ensure_running
    "$DOCKER" logs --tail=200 "$CONTAINER"
}

cmd_artifact() {
    local name="${1:-}"
    if [[ -z "$name" ]]; then
        echo "Usage: $0 artifact <name>" >&2
        echo "Available:" >&2
        "$DOCKER" exec "$CONTAINER" ls -1 "$LOG_DIR" 2>/dev/null >&2 || true
        exit 1
    fi
    ensure_running
    "$DOCKER" cp "$CONTAINER:$LOG_DIR/$name" -
}

cmd_prompt() {
    local prompt="${1:-}"
    if [[ -z "$prompt" ]]; then
        echo "Usage: $0 prompt <text>" >&2
        exit 1
    fi
    cmd_run "" "$prompt"
}

cmd_shell() {
    ensure_running
    "$DOCKER" exec -it "$CONTAINER" bash
}

cmd_stop() {
    "$DOCKER" rm -f "$CONTAINER" 2>/dev/null || true
    echo "stopped."
}

usage() {
    sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//' >&2
    exit 1
}

main() {
    [[ $# -eq 0 ]] && usage
    local cmd="$1"; shift
    case "$cmd" in
        build)    cmd_build "$@" ;;
        run)      cmd_run "$@" ;;
        audit)    cmd_audit "$@" ;;
        logs)     cmd_logs "$@" ;;
        artifact) cmd_artifact "$@" ;;
        prompt)   cmd_prompt "$@" ;;
        shell)    cmd_shell "$@" ;;
        stop)     cmd_stop "$@" ;;
        *)        usage ;;
    esac
}

main "$@"
