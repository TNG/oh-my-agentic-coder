#!/usr/bin/env bash
# Render the real network-approval dialog headless and save a PNG, the same way
# the popup-visual CI job does. Linux only; needs xvfb, imagemagick, xdotool and
# the chosen dialog tool (zenity or kdialog).
#
# Usage: scripts/popup-shot.sh [zenity|kdialog]
set -euo pipefail

backend="${1:-zenity}"
outdir="${OMAC_POPUP_SHOT_DIR:-$(pwd)/popup-shots}"
mkdir -p "$outdir"

OMAC_POPUP_SHOT=1 \
OMAC_POPUP_SHOT_BACKEND="$backend" \
OMAC_POPUP_SHOT_DIR="$outdir" \
  xvfb-run -a --server-args="-screen 0 1280x1024x24" \
  go test ./internal/netprompt/ -run TestRenderPopupScreenshot -count=1 -v

echo "screenshot(s) written to $outdir"
