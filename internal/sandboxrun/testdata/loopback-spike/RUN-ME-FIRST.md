# RUN-ME-FIRST — host-side steps for the loopback spike (Ticket 01)

Everything in this directory is a **fixture written from inside an omac
sandbox where `sandbox-exec` itself is unavailable** (nested sandbox_apply is
EPERM — verified). That has two consequences you must internalize before
trusting any output:

1. **This host run IS the SBPL syntax check.** The profiles have only been
   reviewed by eye; `sandbox-exec -n` dry-run could not be used. If a profile
   has a syntax error, that posture's `sandbox-exec` invocation exits
   immediately with a compile error in its `.log` — that is a *fixture bug to
   fix*, not a finding. Re-run after fixing.
2. **You must run from a host terminal.** Do not run this from inside an omac
   session, an `omac start` shell, tmux-inside-omac, etc. Check first:
   `test -n "$OMAC_SOCKET" && echo STILL-SANDBOXED — ABORT`.

## Prerequisites (one-time, ~1 min)

```bash
sw_vers                                          # record build in the report (seen: 26.5.2 / 25F84)
command -v java && java -version                 # Temurin 25.0.2 seen
command -v sandbox-exec                          # deprecated but expected present
uname -m                                         # arm64 seen
```

Do NOT use `/usr/libexec/java_home` — it fails on this machine (no Apple JDK).
`java` must resolve via PATH (currently via `~/.jenv/shims/java`, pointing at a
Temurin install under the home dir — the profiles allow-read `__HOME__`
precisely so this resolves inside the sandbox).

## Step 1 — the posture matrix

```bash
cd /Users/sajjadtng/Documents/TNG_Other/oh-my-agentic-coder/internal/sandboxrun/testdata/loopback-spike
./run-matrix.sh
```

- Starts a host listener OUTSIDE the sandbox on `127.0.0.1:19211` and
  `[::1]:19211` (override port: `GUARDED_PORT=29211 ./run-matrix.sh`).
- Runs 9 postures: `blocked`, `exact-port`, `dynamic-port`,
  `dynamic-port-deny` (key experiment, deny-after-allow),
  `deny-before-allow` (order control), `dynamic-port-deny-v4` /
  `dynamic-port-deny-v6` (literal-address IPv6-hole probes), `env-only`,
  `open` (controls).
- Prints a summary table at the end.

Output lands in:
`~/.agents/artifacts/github.com--TNG--oh-my-agentic-coder/.scratch/jvm-build-executor/01-loopback/logs/`
- `<posture>.log` — full stdout/stderr incl. every `RESULT ...` line and rc.
- `<posture>.sb` — the exact rendered SBPL used (quote these in REPORT.md).

Sanity anchors before trusting the table:
- `open` and `env-only`: all RESULT lines OK (if not → the *rig* is broken).
- `blocked`: everything FAIL, ideally with `errno=EPERM`.
- `dynamic-port`: guarded-v4 OK (the exposure); external FAIL.
- If EVERY posture shows all-FAIL or logs contain
  `sandbox_apply: Operation not permitted` → you are nested; abort.

## Step 2 — Gradle Worker API reproducer (native-syscall evidence)

```bash
cd ~/.agents/artifacts/github.com--TNG--oh-my-agentic-coder/.scratch/jvm-build-executor/01-loopback/gradle-worker-api
./run-worker.sh dynamic-port-deny        # or any posture name; see script header
```

The script locates the cached Gradle 9.5.1 distribution under
`~/.gradle/wrapper/dists/`, applies the chosen posture profile to the whole
Gradle client→daemon→worker tree, and the worker's WorkAction probes the
same targets as Probe.java (parent build prints its dynamic ports to the log,
mirroring client↔daemon and daemon↔worker loopback).

## Step 3 — native-syscall tracing (sudo/password needed on host)

Primary (dtruss; run on host, will prompt for your password):

```bash
sudo dtruss -f -t connect_nocancel,bind_nocancel \
  /usr/bin/sandbox-exec -f <scratch>/logs/dynamic-port-deny.sb \
  <java-from-run-worker-log> 2> <scratch>/logs/dtruss.log
```

If dtruss is blocked on this macOS build (SIP / dtrace restrictions):

```bash
sudo dtrace -n 'syscall::connect*:entry { printf("%s pid=%d %s", execname, pid, copyinstr(arg1)); }' \
  -o <scratch>/logs/dtrace-connect.log
# then run ./run-worker.sh dynamic-port-deny in another terminal
```

Final fallback if dtrace is entirely unavailable: compare the Java stack
traces (they show `sun.nio.ch.Net.connect0` + errno text) with
`netstat -an | grep LISTEN` deltas, and document the substitution in
REPORT.md per AC4.

## Step 4 — write the report

Assemble `REPORT.md` in the scratch root
(`.../01-loopback/REPORT.md`) per the handoff's required-contents section:
exact SBPL per posture (quote the `.sb` files), raw outcomes table
(child/grandchild/guarded-v4/guarded-v6/egress × posture), the IPv6-hole
finding, the native-syscall evidence (or substitution note), and the go/no-go
verdict on ADR 0003 as written. Every verdict claim must cite log lines.

## If something stalls

- Posture run hangs >70 s: killed automatically; check `.log` tail, re-run
  that single posture with `POSTURES="dynamic-port-deny" ./run-matrix.sh`.
- `java` inside the sandbox won't start (dyld/mach errors): the baseline is
  missing a file-read or mach port. `log show --last 2m --predicate
  'sender == "AppleSandbox" OR process == "sandboxd"'` on the host shows the
  denied operation; widen the profile's baseline (fixtures only — production
  sbpl.go must not be widened as a side effect of this spike).
- Listener start fails with "Address already in use": pick another
  `GUARDED_PORT`.
