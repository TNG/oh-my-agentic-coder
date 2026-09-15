#!/bin/sh
# Usage: sh scripts/publish-nix-release.sh prepare TAG
#        sh scripts/publish-nix-release.sh check|publish TAG SOURCE_REV BASE_REV
# prepare/check are read-only remotely. Only publish creates refs or a PR.
set -eu

die() { printf '%s\n' "$*" >&2; exit 1; }
mode=${1:?expected prepare, check, or publish}
tag=${2:?expected v-prefixed release tag}
case "$mode:$#" in prepare:2|check:4|publish:4) ;; *) die "Invalid arguments" ;; esac
[ "${GITHUB_REPOSITORY:-}" = TNG/oh-my-agentic-coder ] || die "Upstream repository required"
export GH_REPO=TNG/oh-my-agentic-coder GH_HOST=github.com
repo=TNG/oh-my-agentic-coder
python3 - "$tag" <<'PY'
import re, sys
n = r"(?:0|[1-9][0-9]*)"
identifier = rf"(?:{n}|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
semver = rf"v{n}\.{n}\.{n}(?:-{identifier}(?:\.{identifier})*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?"
if not re.fullmatch(semver, sys.argv[1]):
    sys.exit("Expected a v-prefixed SemVer release tag")
PY
version=${tag#v}
nix_tag=nix-$version
branch=automation/$nix_tag
git check-ref-format "refs/tags/$tag"
git check-ref-format "refs/tags/$nix_tag"
git check-ref-format "refs/heads/$branch"

# Fetch into FETCH_HEAD, never force-update even a local release tag.
git fetch --no-tags origin "refs/tags/$tag"
published_source=$(git rev-parse --verify 'FETCH_HEAD^{commit}')
if [ "$mode" = prepare ]; then
    base=$(git rev-parse HEAD)
    source=$published_source
else
    source=$3
    base=$4
fi
python3 - "$source" "$base" <<'PY'
import re, sys
if not all(re.fullmatch(r"[0-9a-f]{40}", arg) for arg in sys.argv[1:]):
    sys.exit("Source and base must be exact 40-character lowercase commit IDs")
PY
[ "$source" = "$published_source" ] || die "Published source tag changed; stop and investigate"
[ "$(git rev-parse HEAD)" = "$base" ] || die "HEAD changed during validation; rerun from main"

release=$(gh release view "$tag" --repo "$repo" --json tagName,isDraft,publishedAt)
printf '%s' "$release" | python3 -c '
import json, sys
r = json.load(sys.stdin)
if r.get("tagName") != sys.argv[1] or r.get("isDraft") is not False or not r.get("publishedAt"):
    sys.exit("Requested release must be published, not draft, and match the exact tag")
' "$tag"

remote_ref() {
    # An empty result means absent; transport errors must not look like absence.
    refs=$(git ls-remote --refs origin "$1") || return 1
    printf '%s' "$refs" | cut -f1
}

check_main() {
    [ "$(remote_ref refs/heads/main)" = "$base" ] || die "main advanced during validation; rerun (no rebase)"
}
check_main

# Parse literal metadata, never evaluate repository-controlled Nix with the PAT.
# For a changed flake, also prove everything outside the four-field block is identical.
inspect_flake() {
    python3 - "$1" "${2:-}" "$version" "$source" <<'PY'
from pathlib import Path
import re, subprocess, sys
target, parent, version, source = sys.argv[1:]
pattern = re.compile(r"\brelease\s*=\s*\{(?P<body>[^{}]*)\};")
def read(ref):
    if ref == "WORKTREE":
        p = Path("flake.nix")
        if p.is_symlink() or not p.is_file() or p.stat().st_mode & 0o111:
            sys.exit("flake.nix must be a regular non-executable file")
        return p.read_text()
    mode = subprocess.check_output(["git", "ls-tree", ref, "--", "flake.nix"], text=True)
    if not mode.startswith("100644 blob "):
        sys.exit("flake.nix must be a regular non-executable file")
    return subprocess.check_output(["git", "show", f"{ref}:flake.nix"], text=True)
def parse(text):
    matches = list(pattern.finditer(text))
    if len(matches) != 1:
        sys.exit("Expected exactly one literal release block")
    m = matches[0]
    fields = re.findall(r'(version|rev|hash|vendorHash)\s*=\s*"([^"\n]+)"\s*;', m['body'])
    remainder = re.sub(r'(version|rev|hash|vendorHash)\s*=\s*"([^"\n]+)"\s*;', '', m['body'])
    if len(fields) != 4 or set(dict(fields)) != {'version', 'rev', 'hash', 'vendorHash'} or remainder.strip():
        sys.exit("Expected only version, rev, hash and vendorHash in release block")
    return dict(fields), text[:m.start()] + 'RELEASE_BLOCK' + text[m.end():]
fields, outside = parse(read(target))
if parent:
    if fields['version'] != version or fields['rev'] != source:
        sys.exit("Release pins do not match requested version/source; never overwrite the Nix tag")
    if outside != parse(read(parent))[1]:
        sys.exit("Only the release block in flake.nix may change")
print(fields['version'])
PY
}

check_version() {
    current=$(inspect_flake "$base")
    python3 - "$current" "$version" <<'PY'
import re, sys
def order(version):
    core, separator, prerelease = version.split('+', 1)[0].partition('-')
    n = r'(?:0|[1-9][0-9]*)'
    identifier = rf'(?:{n}|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
    if not re.fullmatch(rf'{n}\.{n}\.{n}', core) or (separator and not re.fullmatch(rf'{identifier}(?:\.{identifier})*', prerelease)):
        sys.exit('Invalid packaged SemVer; review flake metadata')
    parts = tuple((0, int(p)) if p.isdigit() else (1, p) for p in prerelease.split('.')) if separator else ()
    return tuple(map(int, core.split('.'))), not separator, parts
if order(sys.argv[1]) > order(sys.argv[2]):
    sys.exit(f'main already pins newer version {sys.argv[1]}; refusing release {sys.argv[2]}')
PY
}

existing=$(remote_ref "refs/tags/$nix_tag")
if [ -n "$existing" ]; then
    git fetch --no-tags origin "refs/tags/$nix_tag"
    [ "$(git rev-parse FETCH_HEAD)" = "$existing" ] || die "Nix tag raced; rerun without retagging"
    commit=$(git rev-parse --verify 'FETCH_HEAD^{commit}')
    [ "$(git rev-list --parents -n 1 "$commit" | wc -w | tr -d ' ')" = 2 ] || die "Nix tag must name a packaging-only commit"
    [ "$(git diff-tree --no-commit-id --name-only -r "$commit")" = flake.nix ] || die "Nix tag is not packaging-only"
    inspect_flake "$commit" "$commit^" > /dev/null
    recovery=true
else
    check_version
    recovery=false
fi

if [ "$mode" = prepare ]; then
    [ -z "$(git status --porcelain --untracked-files=all)" ] || die "prepare requires a clean main checkout"
    printf 'base_rev=%s\nsource_rev=%s\nrecovery=%s\n' "$base" "$source" "$recovery" >> "${GITHUB_OUTPUT:?}"
    exit 0
fi

if [ "$recovery" = false ]; then
    python3 - <<'PY'
import subprocess, sys
entries = subprocess.check_output(['git', 'status', '--porcelain', '-z', '--untracked-files=all']).split(b'\0')
if not all(e in (b'', b' M flake.nix', b'M  flake.nix', b'MM flake.nix') for e in entries):
    sys.exit('Only flake.nix may be changed, staged, or untracked')
if entries == [b'']:
    sys.exit('No packaging change to publish')
PY
    inspect_flake WORKTREE "$base" > /dev/null
    git diff --check
    git diff --cached --check
else
    [ -z "$(git status --porcelain --untracked-files=all)" ] || die "Recovery requires a clean main checkout"
fi
[ "$mode" = publish ] || { printf '%s\n' 'Dry validation complete; no refs or PR changed'; exit 0; }
: "${GH_TOKEN:?NIX_RELEASE_TOKEN must be provided as GH_TOKEN for publication}"

if [ "$recovery" = false ]; then
    [ -z "$(remote_ref "refs/heads/$branch")" ] || die "Automation branch already exists without Nix tag; manual recovery required"
    git checkout -b "$branch" "$base"
    git add -- flake.nix
    git -c user.name='omac release automation' \
        -c user.email='omac-release@users.noreply.github.com' \
        -c commit.gpgsign=false commit -m "chore(nix): package $tag" -m 'Refs #285' \
        -m 'Generated by the Nix release workflow.'
    commit=$(git rev-parse HEAD)
    git -c tag.gpgsign=false tag "$nix_tag" "$commit"
    check_main
    # The token exists only in this step's environment, never persisted in Git
    # configuration. The PAT (contents + pull requests write) triggers PR CI.
    # Atomic, non-force push: a tag collision cannot leave a new branch behind.
    # Expand GH_TOKEN inside the credential helper, not into Git's arguments.
    # shellcheck disable=SC2016
    git -c credential.helper= \
        -c 'credential.helper=!f() { printf "username=x-access-token\npassword=%s\n" "$GH_TOKEN"; }; f' \
        push --atomic origin "refs/heads/$branch:refs/heads/$branch" "refs/tags/$nix_tag:refs/tags/$nix_tag"
fi

# The tag is permanent, including after a failed PR request or deleted branch.
# Query all states so retries do not reopen a merged or deliberately closed PR.
prs=$(gh pr list --repo "$repo" --head "$branch" --base main --state all --limit 100 --json state,headRefOid,url)
state=$(printf '%s' "$prs" | python3 -c '
import json, sys
prs = json.load(sys.stdin)
if len(prs) > 1:
    sys.exit("Multiple release PRs found; manual recovery required")
if prs:
    p = prs[0]
    if p["headRefOid"] != sys.argv[1]:
        sys.exit("PR head differs from permanent Nix tag; manual recovery required")
    print(p["state"])
' "$commit")
case "$state" in
    MERGED) printf '%s\n' 'Release PR already merged; permanent tag retained'; exit 0 ;;
    CLOSED) die "Release PR was closed without merging; manual recovery required (do not retag)" ;;
    OPEN|'') ;;
    *) die "Unexpected PR state: $state" ;;
esac
if git merge-base --is-ancestor "$commit" "$base"; then
    printf '%s\n' 'Packaging commit already on main; permanent tag retained'
    exit 0
fi
[ "$(remote_ref "refs/heads/$branch")" = "$commit" ] || die "Automation branch missing or changed; manual recovery required from permanent tag $nix_tag (do not retag)"
[ "$state" != OPEN ] || { printf '%s\n' 'Release PR already open; no changes'; exit 0; }
check_version
check_main
body=$(mktemp)
trap 'rm -f "$body"' EXIT HUP INT TERM
python3 - "$tag" "$source" "$nix_tag" > "$body" <<'PY'
from pathlib import Path
import re, sys
tag, source, nix_tag = sys.argv[1:]
body = re.sub(r'<!--.*?-->', '', Path('.github/pull_request_template.md').read_text(), flags=re.S)
body = body.replace('Closes #NN', 'Refs #285')
sections = {
    'What': f'- Publish the Nix package for `{tag}` at permanent tag `{nix_tag}`.',
    'Why': '- Keep Nix installation pinned to the published release without pushing to main.',
    'How': f'- Packaging-only commit pins source `{source}`; Nix inputs are unchanged.',
    'Verification': f'- Passed `python3 scripts/update-nix-release.py {tag} {source}` (including vendor fetch verification).\n- Passed `EXPECTED_VERSION={tag[1:]} sh scripts/flake_test.sh` and `git diff --check` before tagging.',
    'Follow-up': '- Review and merge into main. Keep the permanent Nix tag even if the automation branch is deleted.',
}
for heading, content in sections.items():
    marker = '## ' + heading
    if marker not in body:
        sys.exit('PR template changed; adapt release automation before retrying')
    body = body.replace(marker, marker + '\n' + content)
print(body.strip() + '\n\nGenerated by the Nix release workflow.')
PY
gh pr create --repo "$repo" --head "$branch" --base main \
    --title "chore(nix): package $tag" --body-file "$body"
