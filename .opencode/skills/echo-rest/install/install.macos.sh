#!/usr/bin/env bash
# Install script for echo-rest (macOS).
#
# omac never runs this for you; it only prints the contents at register time
# so you can inspect + run it yourself. See oh-my-agentic-coder.md §15.
set -euo pipefail

if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 is required; install via: brew install python" >&2
  exit 1
fi

echo "echo-rest: no build step needed; sidecar is a stdlib-only script."
