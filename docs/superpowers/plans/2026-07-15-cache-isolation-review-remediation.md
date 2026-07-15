# Cache Isolation Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve the accepted cache-isolation review findings without broadening production sandbox permissions or changing the cache-scope model.

**Architecture:** Cleanup will create and lock its control files through the already validated `os.Root` descriptor, avoiding pathname mutations after root validation. `doctor` will diagnose Cargo access and excluded host configuration accurately. Cache E2E will explicitly expose only discovered tool runtimes as read-only, while documentation describes the actual cache and environment contract.

**Tech Stack:** Go 1.25 standard library (`os.Root`, `syscall`, `archive/tar`, `compress/gzip`), existing Go unit tests, E2E shell harness, Docker E2E wrapper.

**Spec:** `docs/superpowers/specs/2026-07-15-cache-isolation-review-remediation-design.md`

**Commit policy:** Do not commit unless the user explicitly asks.

---

## File Structure

```text
internal/toolcache/
  clear.go                 # trusted-root cleanup path
  clear_test.go            # replacement-root regression coverage
  lock.go                  # pathname and descriptor-relative lock helpers

internal/cli/
  doctor.go                # Cargo grant and host-sentinel diagnostics
  doctor_test.go           # focused diagnostic behavior tests

internal/e2e/
  cache_isolation_test.go  # tool runtime grants and deterministic cache probes

README.md
oh-my-agentic-coder.md
docs/NONO_SANDBOX.md
openspec/changes/native-sandbox/specs/sandbox-launch/spec.md
```

### Task 1: Lock Cleanup Through the Trusted Root

**Files:**
- Modify: `internal/toolcache/lock.go:16-109`
- Modify: `internal/toolcache/clear.go:140-195`
- Modify: `internal/toolcache/clear_test.go:403-463`

- [ ] **Step 1: Write the root-replacement regression test**

Add `TestClearWorkdirDoesNotTouchReplacementRootWhileAcquiringLock` beside the
existing root-replacement tests. Set up a persistent scope, then arrange a
replacement root that contains a pre-existing lock file with mode `0644` and a
known marker payload. Use a test hook immediately before cleanup acquires its
lock to replace the original root pathname with that replacement.

The assertions must prove that cleanup skips the scope as changed and that the
replacement was not modified:

```go
result, err := ClearWorkdir(workdir)
if err != nil {
	 t.Fatal(err)
}
if result.Status != ClearSkipped || result.Reason != "cache root changed" {
	 t.Fatalf("result = %#v", result)
}
info, err := os.Stat(replacementLock)
if err != nil {
	 t.Fatal(err)
}
if info.Mode().Perm() != 0o644 {
	 t.Fatalf("replacement lock mode = %o, want 0644", info.Mode().Perm())
}
data, err := os.ReadFile(replacementLock)
if err != nil || string(data) != "replacement-marker" {
	 t.Fatalf("replacement lock changed: %q, %v", data, err)
}
```

- [ ] **Step 2: Run the regression test and confirm the current pathname lock fails it**

Run:

```sh
go test -count=1 ./internal/toolcache -run '^TestClearWorkdirDoesNotTouchReplacementRootWhileAcquiringLock$'
```

Expected: the old `acquireLock(trusted.path, ...)` path changes the replacement
lock permissions before `verifyTrustedRoot` reports the swap.

- [ ] **Step 3: Add a descriptor-relative cleanup lock helper**

Keep `acquireLock(rootPath, digest, operation)` unchanged for normal scope
preparation in `cache.go`. Add `acquireRootLock(root *os.Root, digest string,
operation int)` in `lock.go` for cleanup only.

The helper must:

1. Reject invalid digests with `errUnsafeLock`.
2. Open or create `.locks` only relative to `root`; never construct an absolute
   lock path from `trusted.path`.
3. Open the lock file with `locks.OpenFile(digest+".lock", os.O_RDWR|os.O_CREATE,
   0o600)` after `locks.Lstat` rejects an existing symlink or non-regular file.
4. Retain the current `Fstat`, single-link, regular-file, `Flock`, and post-lock
   inode checks, but have the post-lock check use `locks.Lstat(lockName)`.
5. Return the locked `*os.File` so `releaseLock` remains unchanged.

Use descriptor-relative names throughout the new helper:

```go
func acquireRootLock(root *os.Root, digest string, operation int) (*os.File, error) {
	if !isDigest(digest) {
		return nil, fmt.Errorf("%w: invalid digest %q", errUnsafeLock, digest)
	}
	if err := ensureRootPrivateDir(root, ".locks"); err != nil {
		return nil, err
	}
	locks, err := root.OpenRoot(".locks")
	if err != nil {
		return nil, fmt.Errorf("open cache lock directory: %w", err)
	}
	defer locks.Close()
	return openRootLockFile(locks, digest, operation)
}
```

`ensureRootPrivateDir` must use only `root.Lstat`, `root.Mkdir`, `root.Open`,
and descriptor `Chmod`; reject a symlink or non-directory exactly as
`ensurePrivateDir` does for normal locking. Move the existing `rootDeleteHook`
to immediately before `acquireRootLock` and update its comment to describe the
new synchronization point.

Replace this call in `clearTrustedScope`:

```go
lock, err := acquireLock(trusted.path, digest, syscall.LOCK_EX|syscall.LOCK_NB)
```

with:

```go
lock, err := acquireRootLock(trusted.root, digest, syscall.LOCK_EX|syscall.LOCK_NB)
```

- [ ] **Step 4: Run cleanup verification**

Run:

```sh
gofmt -w internal/toolcache/clear.go internal/toolcache/lock.go internal/toolcache/clear_test.go
go test -race -count=1 ./internal/toolcache
```

Expected: the regression test passes, all existing unsafe-lock and active-lock
tests remain green, and no race is reported.

### Task 2: Correct Cargo Diagnostics

**Files:**
- Modify: `internal/cli/doctor.go:257-260,352-468`
- Modify: `internal/cli/doctor_test.go:38-59,157-221`

- [ ] **Step 1: Write failing doctor tests**

Replace `TestDoctorCargoReadNotWarned` with two tests:

```go
func TestDoctorCargoReadWarned(t *testing.T) {
	// A profile Read grant for ~/.cargo must produce a Read advisory.
}

func TestDoctorCargoBinReadNotWarned(t *testing.T) {
	// The narrow ~/.cargo/bin runtime grant must not produce a Cargo-home advisory.
}
```

Add `TestDoctorCargoSentinelsWarnForIsolatedCARGOHome`. Use the existing
`stageHomeWithCargoSentinels` fixture, a profile with only
`filesystem.read: ["~/.cargo/bin"]`, and assert all four sentinel paths appear
while the marker content never appears:

```go
for _, path := range []string{
	"~/.cargo/config", "~/.cargo/config.toml",
	"~/.cargo/credentials", "~/.cargo/credentials.toml",
} {
	if !strings.Contains(output, path) {
		t.Errorf("missing Cargo sentinel warning for %s: %s", path, output)
	}
}
if strings.Contains(output, "SENTINEL_CONTENT") {
	t.Fatal("doctor read Cargo sentinel content")
}
```

- [ ] **Step 2: Run the focused doctor tests and confirm the red state**

Run:

```sh
go test -count=1 ./internal/cli -run '^(TestDoctorCargoReadWarned|TestDoctorCargoBinReadNotWarned|TestDoctorCargoSentinelsWarnForIsolatedCARGOHome)$'
```

Expected: direct Cargo-home reads are not warned and sentinel warnings are
suppressed for the narrow default-like profile.

- [ ] **Step 3: Make grant matching and sentinel reporting precise**

In `matchToolHome`, retain the existing Allow/Write warning for every tool
home. Add a Cargo-only Read warning when `pathsCover(expanded, cargoDir)` is
true. This structural direction warns for `~/.cargo` and parent grants but not
for `~/.cargo/bin`:

```go
if access == "Read" && th.raw == "~/.cargo" && pathsCover(expanded, th.expanded) {
	out = append(out, toolHomeWarning{
		entry: rawEntry,
		access: access,
		impact: "Read access exposes host Cargo configuration and credentials inside the sandbox",
		remediation: th.remediation,
	})
}
```

Remove the separate whole-home Read branch to avoid duplicate Cargo warnings.

Change `cargoSentinelWarnings` to take no profile argument and remove the
`profileGrantsUnder` gate. Call it after any successfully inspected `{{self}}`
sandbox profile, because every such built-in launch uses an isolated
`CARGO_HOME`. Preserve `os.Lstat` and the content-free output.

- [ ] **Step 4: Run doctor verification**

Run:

```sh
gofmt -w internal/cli/doctor.go internal/cli/doctor_test.go
go test -count=1 ./internal/cli
```

Expected: direct `~/.cargo` reads and excluded sentinel files are warned;
`~/.cargo/bin` remains accepted; diagnostics do not include host file contents.

### Task 3: Make Cache E2E Probes Deterministic

**Files:**
- Modify: `internal/e2e/cache_isolation_test.go:18-35,40-66,159-203,237-287,615-819,1013-1095`

- [ ] **Step 1: Add focused failing tests for fixture encoding and ephemeral removal**

Add a non-sandbox test for `serveSourceArchive`'s generated bytes. Open them
with `gzip.NewReader`, then `tar.NewReader`, and require the fixture's
`setup.py` entry. This fails against the current plain-tar `.tar.gz` payload.

Extend `TestE2ECacheEphemeralStartsEmptyAndCleansUp` so the child prints its
cache path and the parent asserts removal after `runOmacShell` returns:

```go
cacheDir := extractEnv(t, out, "OMAC_CACHE_DIR")
if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
	t.Fatalf("ephemeral cache directory remains after child exit: %v", err)
}
```

The child command must also verify the cache begins empty before writing its
marker, preserving the existing start-empty assertion.

- [ ] **Step 2: Add narrow read-only tool-runtime discovery**

Change `writeCacheTestProfile` to accept separate `extraRead` and `extraAllow`
lists. Write `extraRead` to `filesystem.read`; reserve `filesystem.allow` for
writable test-specific paths only.

Add test-local helpers that resolve clean, deduplicated absolute runtime roots:

```go
func commandOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return strings.TrimSpace(string(out))
}

func toolRuntimeReadPaths(t *testing.T) []string {
	t.Helper()
	paths := map[string]struct{}{}
	add := func(path string) {
		if path == "" {
			return
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
			absolute = resolved
		}
		paths[absolute] = struct{}{}
	}
	available := func(name string) bool {
		path, err := exec.LookPath(name)
		if err != nil {
			return false
		}
		add(filepath.Dir(path))
		return true
	}
	if available("go") {
		add(commandOutput(t, "go", "env", "GOROOT"))
	}
	if available("python3") {
		add(commandOutput(t, "python3", "-c", "import sys; print(sys.base_prefix)"))
	}
	if available("npm") {
		add(commandOutput(t, "npm", "root", "-g"))
	}
	if available("cargo") {
		if _, err := exec.LookPath("rustc"); err == nil {
			add(commandOutput(t, "rustc", "--print", "sysroot"))
		}
		add(os.Getenv("RUSTUP_HOME"))
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	slices.Sort(result)
	return result
}
```

Use `exec.LookPath`, `filepath.EvalSymlinks`, and `filepath.Abs` before adding
paths. Log every granted runtime root. Pass the result to the initial tools
profile and the pip profile rewrite that opens the loopback fixture port. Do
not grant cache parents, tool homes, or broad installation parents.

For Cargo, resolve the actual `cargo` executable and `rustc --print sysroot`.
If the host's Cargo invocation requires a host `RUSTUP_HOME`, add only that
resolved toolchain root as read-only and record it in test output; do not grant
the whole test HOME.

- [ ] **Step 3: Add Linux backend availability detection**

Make `sandboxUnavailable` return `(bool, string)`. Retain the macOS socket
length path. On Linux, mirror the integration-test smoke check:

```go
if _, err := exec.LookPath("bwrap"); err != nil {
	return true, "bubblewrap is not installed"
}
if err := exec.Command("bwrap", "--ro-bind", "/", "/", "true").Run(); err != nil {
	return true, "bubblewrap is not functional: " + err.Error()
}
```

Pass the reason to `t.Skipf` in `skipIfSandboxUnavailable`. Keep Docker CI
strict: its existing wrapper check remains a failure if Bubblewrap is absent.

- [ ] **Step 4: Serve a real gzip archive and correct test terminology**

Replace the manual plain-tar byte writer with standard library writers:

```go
var data bytes.Buffer
gz := gzip.NewWriter(&data)
tw := tar.NewWriter(gz)
for _, file := range []struct{ name, body string }{
	{"omac_cache_probe-0.0.1/PKG-INFO", "Metadata-Version: 2.1\nName: omac-cache-probe\nVersion: 0.0.1\n"},
	{"omac_cache_probe-0.0.1/setup.py", "from setuptools import setup\nsetup(name='omac-cache-probe', version='0.0.1', py_modules=['omac_cache_probe'])\n"},
	{"omac_cache_probe-0.0.1/omac_cache_probe.py", "VALUE = 'cache-probe'\n"},
} {
	if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.body))}); err != nil { t.Fatal(err) }
	if _, err := io.WriteString(tw, file.body); err != nil { t.Fatal(err) }
}
if err := tw.Close(); err != nil { t.Fatal(err) }
if err := gz.Close(); err != nil { t.Fatal(err) }
return data.Bytes()
```

Remove the obsolete `tarWriter`, `copyName`, and `copyOctal` helpers. Change
the cache-tools comment from “network-free” to “no external-network dependency”

- [ ] **Step 5: Run focused and full cache E2E checks**

Run:

```sh
gofmt -w internal/e2e/cache_isolation_test.go
go test -tags=e2e -count=1 -run '^TestPipSourceArchiveIsGzip$' ./internal/e2e/
go test -tags=e2e -count=1 -timeout=15m -run '^TestE2ECache' ./internal/e2e/
scripts/e2e-docker.sh cache
```

Expected: the fixture is valid gzip, each available tool can execute under
narrow read grants, cache writes stay within the scope, local missing Bubblewrap
skips with a reason, and Docker treats a missing Bubblewrap as a failure.

### Task 4: Reconcile Documentation With the Implemented Contract

**Files:**
- Modify: `README.md:405-457,490-495,534-551,653-660`
- Modify: `oh-my-agentic-coder.md:456-484,515-517,547-565,692-697,1020,1405-1412`
- Modify: `docs/NONO_SANDBOX.md:59-63,82-109`
- Modify: `openspec/changes/native-sandbox/specs/sandbox-launch/spec.md:35-50,63-80,102-110`

- [ ] **Step 1: Correct cache-boundary claims**

In all public descriptions, replace “every tool’s cache” with the supported
variables: `XDG_CACHE_HOME`, `GOCACHE`, `GOMODCACHE`, `NPM_CONFIG_CACHE`,
`PIP_CACHE_DIR`, and `CARGO_HOME`. State that hardcoded unsupported cache paths
need explicit profile configuration and are not automatically redirected.

State that `~/.rustup` may be read-only visible as a runtime installation path;
it is not writable and is not a cache grant. Preserve the invariant that only
the selected cache scope leaf is writable for caches.

- [ ] **Step 2: Correct no-sandbox and Cargo registry instructions**

Describe `--no-sandbox` as omitting cache-scope creation and cache redirects,
while retaining ordinary OMAC transport variables and the per-launch `TMPDIR`.

Replace every instruction to use a sidecar `env_passthrough` for Cargo tokens
with this pattern:

```text
Export CARGO_REGISTRIES_<NAME>_TOKEN in the environment that starts omac.
If the sandbox profile sets environment.allow_vars, include that exact token
variable name. Cargo receives it as part of the sandboxed harness environment;
sidecar.env_passthrough only configures the sidecar.
```

Keep the project-local `.cargo/config.toml` requirement and clarify that the
registry name follows Cargo's uppercase and dash-to-underscore convention.

- [ ] **Step 3: Correct doctor and external nono guidance**

Describe `omac doctor` as detecting host Cargo sentinel file presence through
`Lstat`, without reading/copying contents, and warning that the isolated Cargo
home will not use them.

For external nono profiles that define `environment.allow_vars`, show the
complete required set:

```yaml
environment:
  allow_vars:
    - OMAC_*
    - XDG_CACHE_HOME
    - GOCACHE
    - GOMODCACHE
    - NPM_CONFIG_CACHE
    - PIP_CACHE_DIR
    - CARGO_HOME
```

Explain that these external profiles do not receive the built-in sandbox
re-exec's trusted reinjection, so omitting a redirect allows a tool to fall
back to its default cache location.

- [ ] **Step 4: Verify documentation consistency**

Run:

```sh
git diff --check
rg -n 'every tool.?s cache|never exposed|env_passthrough.*CARGO|network-free' README.md oh-my-agentic-coder.md docs/NONO_SANDBOX.md openspec/changes/native-sandbox/specs/sandbox-launch/spec.md internal/e2e/cache_isolation_test.go
go test -count=1 ./internal/cli ./internal/sandboxrun ./internal/toolcache
```

Expected: no stale categorical cache claims, Cargo sidecar-token instructions,
or misleading “network-free” label remain; unit packages pass.

### Task 5: Run the Integrated Verification Set

**Files:**
- Verify only: all files from Tasks 1-4

- [ ] **Step 1: Format and inspect the complete diff**

Run:

```sh
gofmt -w internal/toolcache/clear.go internal/toolcache/lock.go internal/toolcache/clear_test.go internal/cli/doctor.go internal/cli/doctor_test.go internal/e2e/cache_isolation_test.go
git diff --check
git diff --stat
```

Expected: no formatting or whitespace errors and changes remain restricted to
the review-remediation files.

- [ ] **Step 2: Run unit and E2E verification**

Run:

```sh
go test -count=1 ./internal/toolcache ./internal/cli ./internal/sandboxrun
go test -race -count=1 ./internal/toolcache
go test -tags=e2e -count=1 -timeout=15m -run '^TestE2ECache' ./internal/e2e/
scripts/e2e-docker.sh cache
```

Expected: all unit tests pass, cache E2E verifies all available tools under the
sandbox, and the Docker suite confirms the Linux Bubblewrap-backed path.

- [ ] **Step 3: Review final behavior against the accepted findings**

Confirm these outcomes before reporting completion:

```text
1  Cargo token docs use profile-allowed harness inheritance, not sidecars.
2  Docs enumerate supported/XDG-aware redirects and read-only Rustup access.
3  Cleanup lock acquisition cannot mutate a replacement root.
4  Tool probes prove runtime visibility before cache assertions.
5  Pip consumes a genuine gzip source archive.
6  Direct Cargo-home Read grants are warned; .cargo/bin is not.
7  Excluded host Cargo files are always reported content-free.
8  No-sandbox docs retain OMAC/TMPDIR injection details.
9  External nono allowlists preserve all cache redirects.
10 Ephemeral scopes are asserted removed after child exit.
11 Linux missing/nonfunctional Bubblewrap skips locally with a reason.
12 Loopback-only pip probes are not labeled network-free.
```
