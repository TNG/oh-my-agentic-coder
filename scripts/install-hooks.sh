#!/usr/bin/env bash
#
# install-hooks.sh — install local git hooks that mirror CI's formatting gate.
#
# CI (.github/workflows/ci.yml) fails on any file not gofmt-clean. This hook
# runs `gofmt -w` on staged .go files and re-stages them, so a commit can
# never introduce gofmt drift in the first place. `go vet`, `staticcheck`,
# build, and tests stay server-side in CI — the hook stays fast.
#
# Usage:
#   scripts/install-hooks.sh
#
# Re-run after cloning or after pulling hook changes. Removes any existing
# pre-commit hook written by this script (identified by its sentinel).

set -euo pipefail

# Resolve repo root via git; falls back to script's parent dir for bare setups.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel 2>/dev/null || printf '%s' "$(cd "$SCRIPT_DIR/.." && pwd)")"
HOOK="$REPO/.git/hooks/pre-commit"
SENTINEL="# omac-install-hooks: pre-commit"

if [ ! -d "$REPO/.git" ]; then
  echo "not at a git worktree root: $REPO" >&2
  exit 1
fi

mkdir -p "$REPO/.git/hooks"

# Replace a previous install; preserve anything else.
if [ -f "$HOOK" ] && ! grep -q "^$SENTINEL" "$HOOK"; then
  echo "existing pre-commit hook is not ours; refusing to overwrite:" >&2
  echo "  $HOOK" >&2
  echo "move it aside and re-run." >&2
  exit 1
fi

cat >"$HOOK" <<'HOOK_EOF'
#!/usr/bin/env bash
# omac-install-hooks: pre-commit
#
# Auto-format staged .go files with gofmt and re-stage them, mirroring the
# gofmt gate in .github/workflows/ci.yml. Fails the commit only if gofmt
# itself errors. Does not run vet/staticcheck/test — those stay in CI to
# keep the hook fast.
set -euo pipefail

staged=$(git diff --cached --name-only --diff-filter=ACM -- '*.go' || true)
[ -z "$staged" ] && exit 0

root=$(git rev-parse --show-toplevel)
changed=0
while IFS= read -r f; do
  [ -n "$f" ] || continue
  before=$(gofmt -l "$f")
  if [ -n "$before" ]; then
    (cd "$root" && gofmt -w "$f")
    git add -- "$f"
    changed=1
  fi
done <<<"$staged"

if [ "$changed" -eq 1 ]; then
  echo "pre-commit: gofmt reformatted and re-staged Go files; review and re-edit commit message if needed."
fi
exit 0
HOOK_EOF

chmod +x "$HOOK"
echo "installed pre-commit hook -> $HOOK"
echo "to remove: rm $HOOK"
