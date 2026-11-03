#!/usr/bin/env bash
#
# e2e-desktop-store.sh — check `omac serve --for-opencode-desktop` against the
# REAL OpenCode Desktop app (Linux, headless). No model, no credentials.
#
# Why: omac reads Desktop's private store (drafts.sqlite) and decides from it
# which folders the sandbox may write. If a Desktop release renames a key or
# changes how it derives project IDs, the unit tests (which use hand-built
# stores) keep passing while the feature silently stops granting folders.
# This script launches the real app, lets it write its store, and asserts:
#
#   1. Desktop writes its store in a form omac reads: the rows of drafts.sqlite
#      (`server` row with usedProjectIDs keyed by "local") or, in 1.18.x, the
#      file opencode.global.dat (server.projects.local);
#   2. omac grants the folder Desktop created on first launch, rejects $HOME,
#      and does not warn about a source the real Desktop wrote;
#   3. a loopback server on another port does not contribute grants, the
#      server omac runs does (planted rows, Desktop stopped);
#   4. a forged opencode.db row for an in-home folder is not granted;
#   5. inside the sandbox the store is masked and cannot be modified.
#
# Everything runs in a throwaway HOME. Nothing outside $WORK is touched.
#
# Usage:
#   scripts/e2e-desktop-store.sh                  # pinned version, needs deps
#   DESKTOP_VERSION=v2.2.5 scripts/e2e-desktop-store.sh
#   scripts/e2e-desktop-store.sh --install-deps   # apt-get the Electron libs + Xvfb (sudo)
#
# Needs: Linux x86_64, bubblewrap, go, curl, python3 (with sqlite3), Xvfb, and
# the Electron runtime libraries (GTK3, NSS, ALSA, gbm, ...). On a fresh
# Ubuntu 24.04 `--install-deps` installs them.
#
# Env:
#   DESKTOP_VERSION  release tag of anomalyco/opencode, or "latest" (default: v2.2.5)
#   OMAC_BIN         prebuilt omac binary (default: built from this checkout)
#   WORK             scratch dir (default: a fresh mktemp -d, removed on exit)
#   KEEP_WORK=1      keep $WORK for inspection
#   DESKTOP_CACHE    dir that keeps the downloaded AppImage between runs
set -u

REPO="$(cd "$(dirname "$0")/.." && pwd)"
DESKTOP_VERSION="${DESKTOP_VERSION:-v2.2.5}"
RELEASE_REPO="anomalyco/opencode"
ASSET="opencode-desktop-linux-x86_64.AppImage"

PASS=0
FAIL=0
ok()   { echo "  [ok]   $1"; PASS=$((PASS+1)); }
bad()  { echo "  [FAIL] $1"; FAIL=$((FAIL+1)); }
hdr()  { echo; echo "== $1 =="; }
die()  { echo "e2e-desktop-store: $*" >&2; exit 2; }

if [ "${1:-}" = "--install-deps" ]; then
  sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
    xvfb libgtk-3-0t64 libatk1.0-0t64 libatk-bridge2.0-0t64 libcups2t64 libcairo2 \
    libpango-1.0-0 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 libgbm1 \
    libasound2t64 libatspi2.0-0t64 libnss3 fonts-dejavu-core || die "apt-get failed"
fi

[ "$(uname -s)" = "Linux" ] && [ "$(uname -m)" = "x86_64" ] || die "Linux x86_64 only"
for c in go curl python3 Xvfb bwrap; do command -v "$c" >/dev/null || die "missing command: $c (try --install-deps)"; done

WORK="${WORK:-$(mktemp -d)}"
DL="$WORK/download"; APP="$WORK/app"; FH="$WORK/home"; SCRIPTS="$WORK/scripts"
mkdir -p "$DL" "$APP" "$FH" "$SCRIPTS"

APP_PID=""; XVFB_PID=""
stop_desktop() {
  [ -n "$APP_PID" ] && kill -TERM -- "-$APP_PID" 2>/dev/null
  # Desktop starts its background service (opencode-cli) detached; it outlives the window.
  pkill -TERM -f "^$FH/.config/ai.opencode.desktop/cli/" 2>/dev/null
  sleep 3
  APP_PID=""
}
cleanup() {
  stop_desktop
  [ -n "$XVFB_PID" ] && kill "$XVFB_PID" 2>/dev/null
  sleep 1
  [ "${KEEP_WORK:-}" = "1" ] || rm -rf "$WORK"
}
trap cleanup EXIT

# --- download, verify, extract (the AppImage is untrusted until its digest matches) ---
if [ "$DESKTOP_VERSION" = "latest" ]; then
  DESKTOP_VERSION="$(curl -fsS -m 30 "https://api.github.com/repos/$RELEASE_REPO/releases/latest" | python3 -I -c "import sys,json; print(json.load(sys.stdin)['tag_name'])")" || die "cannot resolve the latest release"
fi
hdr "fetch OpenCode Desktop $DESKTOP_VERSION"
meta="$(curl -fsS -m 30 "https://api.github.com/repos/$RELEASE_REPO/releases/tags/$DESKTOP_VERSION")" || die "release $DESKTOP_VERSION not found"
want_sha="$(printf '%s' "$meta" | python3 -I -c "
import sys,json
for a in json.load(sys.stdin)['assets']:
    if a['name']=='$ASSET': print((a.get('digest') or '').removeprefix('sha256:'))")"
cached="${DESKTOP_CACHE:+$DESKTOP_CACHE/$DESKTOP_VERSION-$ASSET}"
if [ -n "$cached" ] && [ -f "$cached" ]; then
  cp "$cached" "$DL/$ASSET"
else
  curl -fsSL -m 600 -o "$DL/$ASSET" "https://github.com/$RELEASE_REPO/releases/download/$DESKTOP_VERSION/$ASSET" || die "download failed"
fi
got_sha="$(sha256sum "$DL/$ASSET" | cut -d' ' -f1)"
if [ -n "$want_sha" ] && [ "$want_sha" != "$got_sha" ]; then die "sha256 mismatch for $ASSET"; fi
[ -n "$want_sha" ] && ok "sha256 matches the release digest" || echo "  [warn] release lists no digest; skipping checksum"
if [ -n "$cached" ] && [ ! -f "$cached" ]; then mkdir -p "$DESKTOP_CACHE" && cp "$DL/$ASSET" "$cached"; fi
chmod +x "$DL/$ASSET"
( cd "$APP" && "$DL/$ASSET" --appimage-extract >/dev/null 2>&1 ) || die "AppImage extraction failed"
BIN=""
for name in ai.opencode ai.opencode.desktop; do # 2.x ships ai.opencode, 1.x ai.opencode.desktop
  if [ -f "$APP/squashfs-root/$name" ] && [ -x "$APP/squashfs-root/$name" ] && head -c 4 "$APP/squashfs-root/$name" | grep -q ELF; then BIN="$APP/squashfs-root/$name"; break; fi
done
[ -n "$BIN" ] || die "Electron binary not found in the AppImage"
missing="$(ldd "$BIN" 2>/dev/null | grep -c 'not found')"
[ "$missing" = "0" ] || die "$missing Electron libraries missing (run with --install-deps)"

# --- omac under test ---
hdr "build omac"
OMAC="${OMAC_BIN:-$WORK/omac}"
if [ -z "${OMAC_BIN:-}" ]; then ( cd "$REPO" && go build -o "$OMAC" ./cmd/omac ) || die "go build failed"; fi
ok "omac ready"

# --- launch Desktop headless, isolated HOME. XDG_* must be UNSET: empty values make
# Desktop hang on its splash screen waiting for its background service. ---
hdr "launch Desktop (headless) and let it write its store"
DISP=$((90 + $$ % 100))
Xvfb ":$DISP" -screen 0 1280x800x24 >"$WORK/xvfb.log" 2>&1 &
XVFB_PID=$!
sleep 2
kill -0 "$XVFB_PID" 2>/dev/null || die "Xvfb did not start: $(cat "$WORK/xvfb.log")"
DESKTOP_DIR="$FH/.config/ai.opencode.desktop"
ulimit -c 0 # Electron dumps core when stopped with SIGTERM; nothing to keep
env -u XDG_CONFIG_HOME -u XDG_DATA_HOME -u XDG_STATE_HOME -u XDG_CACHE_HOME \
  HOME="$FH" DISPLAY=":$DISP" setsid "$BIN" --no-sandbox --disable-gpu >"$WORK/desktop.log" 2>&1 </dev/null &
APP_PID=$!

# Helper for the two store forms Desktop has used:
#   sqlite  rows of the `state` table in drafts.sqlite (newer releases; projects as IDs)
#   file    a JSON file opencode.global.dat (1.18.x; projects as folder paths)
cat >"$SCRIPTS/store.py" <<'PY'
import hashlib, json, os, sqlite3, sys

def sha(s): return hashlib.sha1(s.encode()).hexdigest()
def sqlite_row(d):
    try:
        c = sqlite3.connect("file:%s/drafts.sqlite?mode=ro" % d, uri=True)
        r = c.execute("select value from state where name='opencode.global.dat' and key='server'").fetchone()
        return json.loads(r[0]) if r else None
    except Exception:
        return None
def file_data(d):
    try:
        return json.load(open(d + "/opencode.global.dat"))
    except Exception:
        return None
def unwrap(v):                                   # values are often JSON inside a JSON string
    return json.loads(v) if isinstance(v, str) else v
def rewrap(old, v):                              # keep the encoding the file used
    return json.dumps(v) if isinstance(old, str) else v

def detect(d):
    v = sqlite_row(d)
    if v and v.get("usedProjectIDs", {}).get("local"): return "sqlite"
    f = file_data(d)
    if f and unwrap(f.get("server", {})).get("projects", {}).get("local"): return "file"
    return ""

def forge_db(fh, rows):
    """Insert project rows into the real opencode.db, filling whatever NOT NULL columns it has."""
    c = sqlite3.connect(fh + "/.local/share/opencode/opencode.db")
    cols = c.execute("pragma table_info(project)").fetchall()   # cid,name,type,notnull,dflt,pk
    for pid, wt in rows:
        vals = {}
        for _, name, typ, notnull, dflt, pk in cols:
            if name == "id": vals[name] = pid
            elif name == "worktree": vals[name] = wt
            elif notnull and dflt is None: vals[name] = 0 if "int" in typ.lower() else ("[]" if name == "sandboxes" else "")
        c.execute("insert or replace into project (%s) values (%s)" % (",".join(vals), ",".join("?" * len(vals))), list(vals.values()))
    c.commit()

cmd, d = sys.argv[1], sys.argv[2]
if cmd == "detect":
    print(detect(d))
elif cmd == "plant-scope":                      # d = HOME, argv[3] = kind
    fh, kind = d, sys.argv[3]
    dd = fh + "/.config/ai.opencode.desktop"
    names = ("on-served-port", "on-other-port", "forged")
    for n in names: os.makedirs("%s/%s" % (fh, n), exist_ok=True)
    ids = {n: sha("directory:%s/%s" % (fh, n)) for n in names}
    if kind == "sqlite":
        c = sqlite3.connect(dd + "/drafts.sqlite")
        v = sqlite_row(dd)
        v["usedProjectIDs"]["http://localhost:4096"] = [ids["on-served-port"]]   # omac's default serve port
        v["usedProjectIDs"]["http://localhost:3000"] = [ids["on-other-port"]]
        c.execute("update state set value=? where name='opencode.global.dat' and key='server'", (json.dumps(v),)); c.commit()
        forge_db(fh, [(ids["on-served-port"], fh + "/on-served-port"), (ids["on-other-port"], fh + "/on-other-port")])
    else:
        f = file_data(dd)
        srv = unwrap(f["server"])
        srv["projects"]["http://localhost:4096"] = [{"worktree": fh + "/on-served-port"}]
        srv["projects"]["http://localhost:3000"] = [{"worktree": fh + "/on-other-port"}]
        f["server"] = rewrap(f["server"], srv)
        json.dump(f, open(dd + "/opencode.global.dat", "w"))
    forge_db(fh, [("deadbeef", fh + "/forged")])   # an ID the Desktop store never recorded
elif cmd == "plant-secret":                     # d = Desktop dir, argv[3] = kind, argv[4] = secret
    kind, secret = sys.argv[3], sys.argv[4]
    if kind == "sqlite":
        c = sqlite3.connect(d + "/drafts.sqlite")
        c.execute("insert or replace into state (name,key,value,updated_at) values ('opencode.global.dat','e2e.secret',?,0)", (secret,))
        c.commit(); c.execute("pragma wal_checkpoint(truncate)"); c.close()   # secret in the main file; the WAL is a store file too
    else:
        f = file_data(d); f["e2e.secret"] = secret
        json.dump(f, open(d + "/opencode.global.dat", "w"))
PY

KIND=""
for _ in $(seq 1 90); do KIND="$(python3 -I "$SCRIPTS/store.py" detect "$DESKTOP_DIR")"; [ -n "$KIND" ] && break; sleep 1; done
if [ -z "$KIND" ]; then
  bad "Desktop never recorded a project for its built-in server within 90s"
  tail -n 20 "$WORK/desktop.log"
  echo "== summary =="; echo "  PASS=$PASS  FAIL=$FAIL"; exit 1
fi
if [ "$KIND" = "sqlite" ]; then ok "Desktop wrote its store as rows of drafts.sqlite (server row with usedProjectIDs.local)"; else ok "Desktop wrote its store as the file opencode.global.dat (server.projects.local)"; fi

# Stop Desktop: later steps plant rows and must not race its writes.
stop_desktop

# --- omac against the real store ---
cat >"$SCRIPTS/noop.sh" <<'EOF'
#!/bin/sh
echo "[inner ran]"
EOF
chmod +x "$SCRIPTS/noop.sh"
mkdir -p "$FH/work"
serve() { # prints omac's combined output
  ( cd "$FH/work" && env -u XDG_CONFIG_HOME -u XDG_DATA_HOME -u XDG_STATE_HOME HOME="$FH" \
      timeout 90 "$OMAC" serve --for-opencode-desktop --inner "$SCRIPTS/noop.sh" 2>&1 )
}

hdr "omac grants what the real Desktop recorded"
OUT="$(serve)"
DEFAULT_PROJECT="$FH/Default Project"
if echo "$OUT" | grep -qF "r+w $DEFAULT_PROJECT"; then ok "Desktop's 'Default Project' is granted"; else bad "Default Project not granted"; echo "$OUT" | grep -E 'granting|r\+w|skipping|ignoring|rror' | sed 's/^/        /'; fi
if echo "$OUT" | grep -qE "^  r\+w $FH\$"; then bad "\$HOME itself was granted"; else ok "\$HOME itself is not granted"; fi
if echo "$OUT" | grep -q "ignoring invalid OpenCode state source"; then bad "omac warns about a source the real Desktop wrote"; echo "$OUT" | grep "ignoring" | sed 's/^/        /'; else ok "no source was reported as invalid"; fi

hdr "scope: only the built-in server and the server omac runs"
python3 -I "$SCRIPTS/store.py" plant-scope "$FH" "$KIND"
OUT="$(serve)"
if echo "$OUT" | grep -qF "r+w $FH/on-served-port"; then ok "folder on the served port (4096) is granted"; else bad "served-port folder not granted"; fi
if echo "$OUT" | grep -qF "r+w $FH/on-other-port"; then bad "folder from another loopback port was granted"; else ok "folder from another loopback port (3000) is not granted"; fi
if echo "$OUT" | grep -qF "r+w $FH/forged"; then bad "forged opencode.db row was granted"; else ok "forged in-home opencode.db row is not granted"; fi

hdr "inside the sandbox: the real store is unreachable and unchanged"
# Plant a secret in the store (Desktop is stopped). The sandbox must never see it.
SECRET="e2e-secret-$RANDOM$RANDOM"
python3 -I "$SCRIPTS/store.py" plant-secret "$DESKTOP_DIR" "$KIND" "$SECRET"
# What the sandbox sees depends on the layout (the throwaway HOME may be hidden, or
# the store masked), so the probe reports outcomes and the host verifies the result.
cat >"$SCRIPTS/probe.sh" <<PROBE
#!/bin/sh
D="$DESKTOP_DIR"
# 1. disclosure: any way to read the secret out of the store files
n=0
for f in "\$D"/drafts.sqlite "\$D"/drafts.sqlite-wal "\$D"/drafts.sqlite-shm "\$D"/opencode.global.dat; do
  c=\$(cat "\$f" 2>/dev/null | grep -c "$SECRET"); n=\$((n + c))
done
echo "LEAK:\$n"
# 2. legitimate writes must work (checked on the host afterwards)
touch "\$D/opencode/probe-runtime" 2>/dev/null
touch "$FH/Default Project/probe-project" 2>/dev/null
# 3. attacks on the store; all of them must leave the real files as they were
for f in drafts.sqlite drafts.sqlite-wal opencode.global.dat; do
  : >> "\$D/\$f" 2>/dev/null
  printf FORGED > "\$D/\$f" 2>/dev/null
  rm -f "\$D/\$f" 2>/dev/null
done
mv "\$D" "\$D.moved" 2>/dev/null
mv "$FH/.config" "$FH/.config.moved" 2>/dev/null
echo "PROBE-DONE"
PROBE
chmod +x "$SCRIPTS/probe.sh"
# The main files' hashes plus a scan for planted bytes. (-wal/-shm are not compared: omac's
# own read-only open on the host may legitimately create them.)
sums() { sha256sum "$DESKTOP_DIR"/drafts.sqlite "$DESKTOP_DIR"/opencode.global.dat 2>/dev/null; grep -l FORGED "$DESKTOP_DIR"/drafts.sqlite* "$DESKTOP_DIR"/opencode.global.dat 2>/dev/null; }
BEFORE="$(sums)"
OUT="$( cd "$FH/work" && env -u XDG_CONFIG_HOME -u XDG_DATA_HOME -u XDG_STATE_HOME HOME="$FH" timeout 90 "$OMAC" serve --for-opencode-desktop --inner "$SCRIPTS/probe.sh" 2>&1 )"
if echo "$OUT" | grep -q "PROBE-DONE"; then ok "probe ran inside the sandbox"; else bad "probe did not complete"; echo "$OUT" | tail -n 8 | sed 's/^/        /'; fi
if echo "$OUT" | grep -q "^LEAK:0$"; then ok "the sandbox cannot read the store's contents"; else bad "store contents were readable: $(echo "$OUT" | grep '^LEAK:')"; fi
if [ "$(sums)" = "$BEFORE" ]; then ok "the store files are byte-identical and hold no planted bytes"; else bad "a store file changed or contains planted bytes"; fi
if [ -d "$DESKTOP_DIR" ] && [ ! -e "$DESKTOP_DIR.moved" ] && [ ! -e "$FH/.config.moved" ]; then ok "the Desktop data dir and its parent were not moved"; else bad "a Desktop directory was moved"; fi
if [ -f "$DESKTOP_DIR/opencode/probe-runtime" ]; then ok "OpenCode's runtime subtree is writable"; else bad "runtime subtree is not writable from the sandbox"; fi
if [ -f "$FH/Default Project/probe-project" ]; then ok "a granted project folder is writable"; else bad "granted project is not writable"; echo "$OUT" | grep -E 'granting|r\+w|skipping|ignoring' | sed 's/^/        /'; fi

echo; echo "== summary =="; echo "  PASS=$PASS  FAIL=$FAIL"
[ "$FAIL" = "0" ] || exit 1
