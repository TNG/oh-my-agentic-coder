#!/usr/bin/env bash
# Install script for echo-rest (Linux).
set -euo pipefail

if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 is required; install via: sudo apt-get install -y python3  # or dnf/pacman" >&2
  exit 1
fi

echo "echo-rest: no build step needed; sidecar is a stdlib-only script."
