#!/usr/bin/env bash
# Pop up the real omac network-access dialog to inspect its appearance on this
# platform. Drives the production netprompt code path (cmd/netprompt-demo), so
# changes to the dialog code — window size, labels, prompt text — show up here
# automatically.
#
# Usage:
#   scripts/netprompt-popup-demo.sh [-host H] [-port P] [-intent TEXT]
#
# Window size defaults to 520x560; override with OMAC_PROMPT_WIDTH /
# OMAC_PROMPT_HEIGHT (e.g. on a small/720p screen or a tiling WM).
#
# Requires a dialog backend: zenity or kdialog on Linux, osascript on macOS.
set -euo pipefail
cd "$(dirname "$0")/.."
exec go run ./cmd/netprompt-demo "$@"
