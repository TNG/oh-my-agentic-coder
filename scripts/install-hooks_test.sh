#!/usr/bin/env bash
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
INSTALLER="$ROOT/scripts/install-hooks.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

init_repo() {
  local repo="$1"
  git init -q "$repo"
  git -C "$repo" config user.email test@example.com
  git -C "$repo" config user.name test
  mkdir -p "$repo/scripts"
  cp "$INSTALLER" "$repo/scripts/install-hooks.sh"
  chmod +x "$repo/scripts/install-hooks.sh"
  touch "$repo/README"
  git -C "$repo" add .
  git -C "$repo" commit -qm initial
}

assert_omac_hook() {
  local hook="$1"
  [ -x "$hook" ] || fail "hook is not executable: $hook"
  grep -q '^# omac-install-hooks: pre-commit' "$hook" || fail "missing omac hook sentinel: $hook"
}

normal="$TMP/normal"
init_repo "$normal"
"$normal/scripts/install-hooks.sh"
assert_omac_hook "$normal/.git/hooks/pre-commit"
printf 'package main\nfunc main(){}\n' >"$normal/main.go"
git -C "$normal" add main.go
git -C "$normal" commit -qm format
[ -z "$(gofmt -l "$normal/main.go")" ] || fail "hook did not format staged Go file"

linked="$TMP/linked"
git -C "$normal" worktree add -qb linked "$linked"
"$linked/scripts/install-hooks.sh"
linked_hook="$(git -C "$linked" rev-parse --path-format=absolute --git-path hooks/pre-commit)"
assert_omac_hook "$linked_hook"

relative="$TMP/relative"
init_repo "$relative"
git -C "$relative" config core.hooksPath .githooks
"$relative/scripts/install-hooks.sh"
assert_omac_hook "$relative/.githooks/pre-commit"

absolute="$TMP/absolute"
init_repo "$absolute"
absolute_hooks="$TMP/absolute-hooks"
git -C "$absolute" config core.hooksPath "$absolute_hooks"
"$absolute/scripts/install-hooks.sh"
assert_omac_hook "$absolute_hooks/pre-commit"

protected="$TMP/protected"
init_repo "$protected"
printf '#!/usr/bin/env bash\nexit 0\n' >"$protected/.git/hooks/pre-commit"
chmod +x "$protected/.git/hooks/pre-commit"
if "$protected/scripts/install-hooks.sh" >/dev/null 2>&1; then
  fail "installer overwrote a non-omac hook"
fi
grep -q '^exit 0$' "$protected/.git/hooks/pre-commit" || fail "non-omac hook changed"

printf 'PASS: install-hooks supports normal clones, worktrees, and core.hooksPath\n'
