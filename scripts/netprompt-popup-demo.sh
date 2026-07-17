#!/usr/bin/env bash
# Pop up the real omac network-access dialog to inspect its appearance on this
# platform. Drives the production netprompt code path (cmd/netprompt-demo), so
# changes to the dialog code — window size, labels, prompt text — show up here
# automatically.
#
# Usage:
#   scripts/netprompt-popup-demo.sh [-host H] [-port P] [-intent TEXT]
#
# Requires a dialog backend: zenity or kdialog on Linux, osascript on macOS.
set -euo pipefail
cd "$(dirname "$0")/.."
exec go run ./cmd/netprompt-demo "$@"
