# Sandbox Tool Cache Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace broad host-cache grants with persistent trust-domain caches, an opt-in ephemeral mode, safe manual cleanup, and clear diagnostics without breaking supported harnesses or developer tools.

**Architecture:** A new `internal/toolcache` package owns canonical scope identity, full-digest paths, environment mappings, lifetime locks, and safe cleanup. CLI launch paths prepare one cache scope, grant only its leaf, and pass a validated cache contract through the built-in sandbox re-exec; profile, doctor, provenance, briefing, E2E, and documentation changes consume that same package rather than duplicating cache rules.

**Tech Stack:** Go 1.25, standard library only, `syscall.Flock` on supported Linux/macOS platforms, Seatbelt, bubblewrap, existing Go test/E2E infrastructure.

**Spec:** `docs/superpowers/specs/2026-07-10-cache-isolation-design.md`

**Worktree:** Execute in the current dedicated worktree. Do not create another nested worktree.

**Commit policy:** Do not commit unless the user explicitly requests commits during execution. Keep each task independently testable so commits can be added later without reorganizing changes.

---

## File Structure

```text
internal/toolcache/
  cache.go                 # Modes, domains, canonical identity, layout, env mappings
  cache_test.go
  lock.go                  # Shared launch locks and non-blocking exclusive cleanup locks
  clear.go                 # Current/all cache deletion with validation
  clear_test.go

internal/cli/
  start.go                 # start/continue/resume flag, cache preparation, grant/env injection
  serve.go                 # single-dir/serve scope selection and static cache env
  cache.go                 # omac cache clear [--all]
  cache_test.go
  briefing.go              # append dynamic cache guidance to resolved briefing
  briefing_test.go
  provenance.go            # cache section in text/JSON output
  provenance_test.go
  doctor.go                # legacy profile and excluded Cargo-config warnings
  doctor_test.go
  cli.go                   # cache command registration/help

internal/sandboxrun/
  run.go                   # validate/re-inject cache contract after environment filtering
  run_test.go
  cache_integration_linux_test.go
  cache_integration_darwin_test.go

internal/sandboxprofile/
  resolve.go               # safe compiled defaults and read-only resolver for doctor
  profile_test.go

internal/config/
  harness.go               # remove harness-global cache grants
  harness_test.go
  launcher.go              # correct stale broad-profile equivalence comments

internal/sandboxbrief/
  sandboxbrief.go          # dynamic cache guidance renderer
  sandboxbrief_test.go

internal/e2e/
  cache_isolation_test.go  # deterministic no-model launch/tool tests
  testdata/rust-cache-probe/Cargo.toml
  testdata/rust-cache-probe/src/main.rs
  e2e_test.go              # production-like profile fixture
  harnesses.go             # corrected cache comments
  harnesses_test.go        # platform-correct harness expectations
  provenance_test.go       # cache provenance contract

README.md
oh-my-agentic-coder.md
docs/MULTI_DIR_DESKTOP.md
docs/NONO_SANDBOX.md
openspec/changes/native-sandbox/specs/sandbox-launch/spec.md
.github/workflows/ci.yml
Dockerfile.e2e
scripts/e2e-docker.sh
AGENTS.md
```

### Shared Contracts

Use these public shapes consistently across all tasks:

```go
package toolcache

type Mode string
const (
	ModePersistent Mode = "persistent"
	ModeEphemeral  Mode = "ephemeral"
)

type Domain string
const (
	DomainWorkdir Domain = "workdir"
	DomainServe   Domain = "serve"
)

type Scope struct {
	Domain        Domain
	Mode          Mode
	CanonicalPath string
	Identity      string
	Digest        string
	Dir           string
	lock          *os.File
}

func DescribePersistent(domain Domain, path string) (Scope, error)
func PreparePersistent(domain Domain, path string) (*Scope, error)
func PrepareEphemeral(sandboxTmp string) (*Scope, error)
func Environment(dir string, mode Mode) map[string]string
func (s *Scope) Close() error
```

The exact mapping is:

```go
map[string]string{
	"XDG_CACHE_HOME":  filepath.Join(dir, "xdg"),
	"GOCACHE":         filepath.Join(dir, "go-build"),
	"GOMODCACHE":      filepath.Join(dir, "go-mod"),
	"NPM_CONFIG_CACHE": filepath.Join(dir, "npm"),
	"PIP_CACHE_DIR":   filepath.Join(dir, "pip"),
	"CARGO_HOME":      filepath.Join(dir, "cargo"),
	"OMAC_CACHE_DIR":  dir,
	"OMAC_CACHE_MODE": string(mode),
}
```

---

### Task 1: Cache Identity, Layout, and Environment

**Files:**
- Create: `internal/toolcache/cache.go`
- Create: `internal/toolcache/cache_test.go`

- [ ] **Step 1: Write failing identity and environment tests**

Create table-driven tests named:

```go
func TestDescribePersistentCanonicalAliasesShareIdentity(t *testing.T)
func TestDescribePersistentMainAndLinkedWorktreesDiffer(t *testing.T)
func TestDescribePersistentDomainsDiffer(t *testing.T)
func TestDescribePersistentUsesFullSHA256(t *testing.T)
func TestEnvironment(t *testing.T)
func TestPreparePersistentPermissionsAndSafety(t *testing.T)
func TestPrepareEphemeralBypassesPersistentState(t *testing.T)
```

For the worktree test, initialize a real repository, commit one file with
`git -c user.name=Omac -c user.email=omac@example.invalid commit`, and run
`git worktree add -q linkedDir -b linked-cache-test`; assert only that canonical
paths differ and cache identities differ. Do not inspect `.git` to derive the
identity. For aliases, create a symlink to one worktree and assert identical
identity/digest.

The environment assertion must compare all eight exact entries from the shared
contract above. Permission tests set `HOME` to a temporary home, assert
`~/.cache/omac` and the 64-hex scope leaf are `0700`, and assert a symlink or
regular file at the scope path returns an error.

- [ ] **Step 2: Run the tests and confirm the red state**

Run: `go test -count=1 ./internal/toolcache`

Expected: FAIL because `internal/toolcache` and its types do not exist.

- [ ] **Step 3: Implement the minimal cache package**

Implement `cache.go` with these rules:

```go
func DescribePersistent(domain Domain, path string) (Scope, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil { return Scope{}, err }
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil { return Scope{}, err }
	identity := "v1:" + string(domain) + ":" + canonical
	sum := sha256.Sum256([]byte(identity))
	digest := hex.EncodeToString(sum[:])
	home, err := os.UserHomeDir()
	if err != nil { return Scope{}, err }
	return Scope{
		Domain: domain, Mode: ModePersistent, CanonicalPath: canonical,
		Identity: identity, Digest: digest,
		Dir: filepath.Join(home, ".cache", "omac", digest),
	}, nil
}
```

Add an `ensurePrivateDir` helper using `os.Lstat`; reject symlinks and
non-directories, create missing directories, and call `os.Chmod(path, 0o700)`
so existing permissive directories are tightened. `PreparePersistent` must
validate/create `~/.cache/omac`, then prepare the scope leaf. Do not truncate
the SHA-256 digest. `PrepareEphemeral` creates `filepath.Join(sandboxTmp, "cache")` with `0700`,
sets `ModeEphemeral`, and does not resolve `HOME`, canonicalize a workdir, or
touch the persistent root.

- [ ] **Step 4: Run package tests and formatting**

Run: `gofmt -w internal/toolcache/cache.go internal/toolcache/cache_test.go`

Run: `go test -count=1 ./internal/toolcache`

Expected: PASS.

---

### Task 2: Lifetime Locks and Safe Cleanup

**Files:**
- Create: `internal/toolcache/lock.go`
- Create: `internal/toolcache/clear.go`
- Create: `internal/toolcache/clear_test.go`
- Modify: `internal/toolcache/cache.go`

- [ ] **Step 1: Write failing lock and cleanup tests**

Add tests:

```go
func TestPreparePersistentHoldsSharedLock(t *testing.T)
func TestClearWorkdirRemovesInactiveScope(t *testing.T)
func TestClearWorkdirRefusesActiveScope(t *testing.T)
func TestClearAllRemovesOnlyDigestDirectories(t *testing.T)
func TestClearAllSkipsActiveAndUnsafeEntries(t *testing.T)
func TestClearDoesNotFollowSymlink(t *testing.T)
```

Use these result contracts:

```go
type ClearStatus string
const (
	ClearRemoved ClearStatus = "removed"
	ClearActive  ClearStatus = "active"
	ClearSkipped ClearStatus = "skipped"
)
type ClearResult struct {
	Path   string
	Status ClearStatus
	Reason string
}
func ClearWorkdir(path string) (ClearResult, error)
func ClearAll() ([]ClearResult, error)
```

Create malformed names, a 64-hex symlink to an outside directory, a regular
file, an inactive valid scope, and an active valid scope. Assert outside data
survives and active scopes are not deleted.

- [ ] **Step 2: Verify tests fail**

Run: `go test -count=1 ./internal/toolcache`

Expected: FAIL with undefined cleanup and lock APIs.

- [ ] **Step 3: Implement lock ordering and deletion validation**

Implement lock files at `filepath.Join(root, ".locks", digest+".lock")`, with `.locks`
mode `0700` and files mode `0600`. `PreparePersistent` must acquire
`LOCK_SH` before validating/creating the scope leaf and retain the file in
`Scope.lock` until `Close`. Cleanup uses `LOCK_EX|LOCK_NB`.

Only immediate children matching `^[0-9a-f]{64}$` are deletion candidates.
Use `os.Lstat`, skip symlinks and non-directories, and never pass an
unvalidated path to `os.RemoveAll`. `ClearWorkdir` uses `DomainWorkdir` and the
same canonical identity. `ClearAll` skips `.locks`, malformed entries, unsafe
entries, and active scopes while returning one result per encountered scope.
Before opening a lock file, reject an existing symlink or non-regular file;
open with `syscall.O_NOFOLLOW` on Linux/macOS and verify the descriptor with
`Fstat`. Add tests for a symlinked lock file, lock-inode replacement, and
successful exclusive acquisition after `Scope.Close`.

- [ ] **Step 4: Verify package tests**

Run: `gofmt -w internal/toolcache/*.go`

Run: `go test -race -count=1 ./internal/toolcache`

Expected: PASS with no race reports.

---

### Task 3: Validated Cache Environment Across the Sandbox Re-exec

**Files:**
- Modify: `internal/sandboxrun/run.go`
- Create or Modify: `internal/sandboxrun/run_test.go`

- [ ] **Step 1: Write failing validation/filter tests**

Extract and test:

```go
func injectedToolCacheEnv(grants *Grants, getenv func(string) string) (map[string]string, error)
```

Tests must prove:

1. Empty `OMAC_CACHE_DIR` returns no cache injection.
2. `OMAC_CACHE_MODE` accepts only `persistent` or `ephemeral`.
3. The cleaned cache directory must exactly match one of `grants.AllowPaths`.
4. Tool values are regenerated from `OMAC_CACHE_DIR`; hostile inherited
   `GOCACHE` or `CARGO_HOME` values are ignored.
5. Passing the returned map to `sandboxprofile.FilterEnv` preserves all eight
   values even when `allow_vars` excludes them.

- [ ] **Step 2: Verify the red state**

Run: `go test -count=1 ./internal/sandboxrun -run 'TestInjectedToolCacheEnv|TestCacheEnvSurvivesAllowVars'`

Expected: FAIL because the helper does not exist and cache values are currently
ordinary inherited environment variables.

- [ ] **Step 3: Implement trusted regeneration**

After `ResolveGrants` and before proxy variables are added, call the helper and
merge its returned values into `injected`. Resolve path equality using cleaned,
absolute paths and `filepath.EvalSymlinks` for existing paths. Return a launch
error if the mode is invalid or the cache directory lacks an exact writable
grant. Do not permit arbitrary environment names to bypass `allow_vars`.

The outer launcher will still set all eight variables for external sandbox
profiles; the built-in sandbox treats only the validated directory/mode pair as
the contract and regenerates the remaining six tool paths.

- [ ] **Step 4: Run tests**

Run: `gofmt -w internal/sandboxrun/run.go internal/sandboxrun/run_test.go`

Run: `go test -count=1 ./internal/sandboxrun ./internal/sandboxprofile`

Expected: PASS.

---

### Task 4: Start, Continue, and Resume Integration

**Files:**
- Modify: `internal/cli/start.go`
- Modify: `internal/cli/continue_resume_test.go`

- [ ] **Step 1: Write failing launch-option and setup tests**

Add `ephemeralCache bool` to expected `launchOpts` values and test:

```go
func TestParseLaunchArgsEphemeralCache(t *testing.T)
func TestParseLaunchArgsRejectsEphemeralWithoutSandbox(t *testing.T)
func TestBuildContinueOptsPreservesEphemeralCache(t *testing.T)
func TestLaunchCachePersistentAndEphemeral(t *testing.T)
```

The setup test should use a small helper extracted from `runLaunch`:

```go
func prepareLaunchCache(noSandbox, ephemeral bool, workdir, sandboxTmp string) (*toolcache.Scope, error)
```

Assert no-sandbox returns `nil`, persistent mode uses `DomainWorkdir`, and
ephemeral mode creates `filepath.Join(sandboxTmp, "cache")` without touching `~/.cache/omac`.

- [ ] **Step 2: Run focused tests and confirm failure**

Run: `go test -count=1 ./internal/cli -run 'Test(ParseLaunchArgs|BuildContinueOpts|LaunchCache)'`

Expected: FAIL because the flag, option, and helper are absent.

- [ ] **Step 3: Wire cache preparation into `runLaunch`**

Register `--ephemeral-cache` in `parseLaunchArgs`; reject its combination with
`--no-sandbox` before `runLaunch`. After `sandboxTmp` exists and before sidecars
start, prepare the cache, defer `Close`, and on persistent setup errors append:

```text
retry with --ephemeral-cache to bypass persistent cache setup
```

For sandboxed launches:

```go
argv = injectSandboxFlag(argv, "--allow", scope.Dir)
for k, v := range toolcache.Environment(scope.Dir, scope.Mode) {
	extra[k] = v
}
```

Print `[verbose] cache mode=%s path=%s` only in verbose mode. Thread the
actual selected scope through `runLaunch` so Task 8 can add it to briefing
construction. Ordinary `--no-sandbox` keeps the current host environment and
performs no cache setup.

- [ ] **Step 4: Verify shared launch tests**

Run: `gofmt -w internal/cli/start.go internal/cli/continue_resume_test.go`

Run: `go test -count=1 ./internal/cli -run 'Test(ParseLaunchArgs|BuildContinueOpts|BuildResume|LaunchCache)'`

Expected: PASS.

---

### Task 5: Serve Trust Domains and Cache Integration

**Files:**
- Modify: `internal/cli/serve.go`
- Modify: `internal/cli/serve_test.go`

- [ ] **Step 1: Write failing serve scope tests**

Extract a pure selector:

```go
func prepareServeCache(noSandbox, noInner, ephemeral bool, explicitWorkdir, launchWorkdir, sandboxTmp string) (*toolcache.Scope, error)
```

Test that:

- `--no-inner` and ordinary `--no-sandbox` return `nil`.
- `--ephemeral-cache --no-sandbox` is rejected during argument validation.
- `serve --workdir /path/to/project` uses `DomainWorkdir` and that explicit directory.
- multi-directory/Desktop serve uses `DomainServe` and `env.Workdir`.
- ephemeral serve uses the temp cache regardless of project roots.
- `serveServer.baseEnv()` contains all eight exact cache values.

- [ ] **Step 2: Confirm tests fail**

Run: `go test -count=1 ./internal/cli -run 'Test.*Serve.*Cache|TestBaseEnvStaticVars'`

Expected: FAIL because serve has no cache mode or scope.

- [ ] **Step 3: Implement serve integration**

Add `--ephemeral-cache`, prepare the selected scope after `sandboxTmp` creation,
hold its lock until the inner server exits, inject only `--allow` plus `scope.Dir`,
and store the generated environment on `serveServer` for `baseEnv`. Build the
briefing-facing scope data only after cache selection so Task 8 can render the
actual mode/path. Preserve the distinction between top-level
`omac --workdir` and serve's own `--workdir` option.

- [ ] **Step 4: Verify serve tests**

Run: `gofmt -w internal/cli/serve.go internal/cli/serve_test.go`

Run: `go test -count=1 ./internal/cli -run 'Test.*Serve|TestBaseEnvStaticVars'`

Expected: PASS.

---

### Task 6: Safe Defaults and Harness Grants

**Files:**
- Modify: `internal/sandboxprofile/resolve.go`
- Modify: `internal/sandboxprofile/profile_test.go`
- Modify: `internal/config/harness.go`
- Modify: `internal/config/harness_test.go`

- [ ] **Step 1: Change tests first**

Update the four `TestSandboxDirs*` expectations to remove only:

```text
~/.cache/opencode
~/.cache/claude
~/.cache/codex
~/.cache/copilot
```

Add `TestDefaultProfileIsolatesToolCaches`. Assert `~/.cache` and
`~/Library/Caches` are absent from `Filesystem.Allow`, `Filesystem.Read`, and
`Filesystem.Write`; `~/go`, `~/.cargo`, and `~/.rustup` are absent from both
writable fields; whole `~/go` and `~/.cargo` are absent from
`Filesystem.Read`; and
`Filesystem.Read` contains `~/.nvm`, `~/.bun/bin`, `~/.cargo/bin`,
`~/.rustup`, and `~/go/bin`. Keep `TestResolveExistingDefaultWins` unchanged
to prove existing profiles are not rewritten.

- [ ] **Step 2: Run focused tests and see expected failures**

Run: `go test -count=1 ./internal/config ./internal/sandboxprofile`

Expected: FAIL because current defaults and harness descriptors still contain
the broad grants.

- [ ] **Step 3: Apply the descriptor/profile changes atomically after launch support exists**

Change `DefaultProfile` exactly as tested. Keep harness config/auth/session
directories read-write; remove only the cache entries. Update comments so
`SandboxDirs` describes runtime config/state/session data, not caches. At this
point Tasks 3-5 already provide the replacement cache environment and grant, so
new profiles never enter a cache-less intermediate state.

- [ ] **Step 4: Verify profile, config, and launch tests together**

Run: `gofmt -w internal/sandboxprofile/resolve.go internal/sandboxprofile/profile_test.go internal/config/harness.go internal/config/harness_test.go`

Run: `go test -count=1 ./internal/config ./internal/sandboxprofile ./internal/sandboxrun ./internal/cli`

Expected: PASS.

---

### Task 7: Manual Cache Cleanup CLI

**Files:**
- Create: `internal/cli/cache.go`
- Create: `internal/cli/cache_test.go`
- Modify: `internal/cli/cli.go`

- [ ] **Step 1: Write failing command tests**

Add:

```go
func TestRunCacheClearCurrent(t *testing.T)
func TestRunCacheClearCurrentActive(t *testing.T)
func TestRunCacheClearAllReportsRemovedAndSkipped(t *testing.T)
func TestRunCacheClearRejectsUnknownArguments(t *testing.T)
func TestCacheCommandRegistered(t *testing.T)
```

Use `env.Workdir` to prepare the current scope. Assert active is reported and
returns success without deletion; real I/O errors return `ExitIOError`;
unknown verbs/flags return `ExitMisuse`.

- [ ] **Step 2: Verify tests fail**

Run: `go test -count=1 ./internal/cli -run 'TestRunCache|TestCacheCommand'`

Expected: FAIL because `runCache` is undefined and `cache` is unregistered.

- [ ] **Step 3: Implement `omac cache clear [--all]`**

Dispatch only the `clear` verb. Without `--all`, call
`toolcache.ClearWorkdir(env.Workdir)`. With `--all`, call
`toolcache.ClearAll()`. Render each result as `removed`, `active`, or `skipped`
with path and reason. Do not add an interactive prompt; `--all` is explicit
destructive confirmation. Add the subcommand to `commands()` and top-level
usage.

- [ ] **Step 4: Verify command tests**

Run: `gofmt -w internal/cli/cache.go internal/cli/cache_test.go internal/cli/cli.go`

Run: `go test -count=1 ./internal/cli -run 'TestRunCache|TestCacheCommand'`

Expected: PASS.

---

### Task 8: Briefing and Provenance Observability

**Files:**
- Modify: `internal/sandboxbrief/sandboxbrief.go`
- Modify: `internal/sandboxbrief/sandboxbrief_test.go`
- Modify: `internal/cli/start.go`
- Modify: `internal/cli/serve.go`
- Modify: `internal/cli/briefing.go`
- Modify: `internal/cli/briefing_test.go`
- Modify: `internal/cli/provenance.go`
- Modify: `internal/cli/provenance_test.go`

- [ ] **Step 1: Write failing briefing tests**

Define:

```go
func CacheGuidance(dir string, mode toolcache.Mode) string
```

Assert the rendered paragraph names the actual path/mode,
`OMAC_CACHE_DIR`, `OMAC_CACHE_MODE`, all tool-specific variables, and explains
that hardcoded host caches are denied. Assert `briefingInjection` appends this
paragraph after both default and custom general briefing text, while preserving
existing no-sandbox/non-harness skips.

- [ ] **Step 2: Write failing provenance tests**

Extend the payload with:

```go
type cacheView struct {
	Scope       string            `json:"scope"`
	Mode        string            `json:"mode"`
	Path        string            `json:"path"`
	Environment map[string]string `json:"environment"`
}
```

Assert text and JSON describe the current workdir's default persistent scope
using `toolcache.DescribePersistent`, without creating its directory. Include
all eight mappings and label this as a default, not a live ephemeral/serve
process.

- [ ] **Step 3: Run tests and confirm failure**

Run: `go test -count=1 ./internal/sandboxbrief ./internal/cli -run 'Test.*(Brief|CacheSection|Provenance.*Cache)'`

Expected: FAIL because cache guidance and cache provenance do not exist.

- [ ] **Step 4: Implement both observability paths**

Keep `brief.md` as the static general briefing; generate cache text dynamically
from the actual scope selected in Tasks 4 and 5 so user overrides cannot
suppress it. Update both `runLaunch` and `runServe` call sites. Add
`Cache cacheView` to
`provenanceView`, render a `CACHE` section in text mode, and let JSON marshal
the new field. Do not duplicate hashing or environment mappings in CLI code.

- [ ] **Step 5: Verify observability tests**

Run: `gofmt -w internal/sandboxbrief/*.go internal/cli/briefing.go internal/cli/briefing_test.go internal/cli/provenance.go internal/cli/provenance_test.go`

Run: `go test -count=1 ./internal/sandboxbrief ./internal/cli`

Expected: PASS.

---

### Task 9: Doctor Warnings Without Profile Mutation

**Files:**
- Modify: `internal/sandboxprofile/resolve.go`
- Modify: `internal/sandboxprofile/profile_test.go`
- Modify: `internal/cli/doctor.go`
- Create: `internal/cli/doctor_test.go`

- [ ] **Step 1: Add a read-only profile resolver test**

Introduce:

```go
func ResolveReadOnly(ref string) (*Profile, string, error)
```

Test that a missing named profile errors, an existing profile loads, and a
missing `default` returns `DefaultProfile()` without creating
`~/.config/omac/sandbox-profiles/default.json`.

- [ ] **Step 2: Add failing doctor rule tests**

Cover component-aware parent grants and exact field semantics:

```text
cache roots: warn for Allow, Read, Write
~/go: warn for Allow/Write, not Read
~/.cargo: warn for Allow/Write and whole-home Read; not ~/.cargo/bin Read
~/.rustup: warn for Allow/Write; not Read
```

Also test parent paths such as `$HOME`, no false match for `.cargo2`, opaque
external launcher commands are skipped, warnings remain exit-code-neutral, and
existing profiles are unchanged.

Recognize all inspectable built-in forms: `--profile default`,
`--profile=default`, omitted `--profile` (which resolves `default`), and an
explicit profile path. Only non-`{{self}} sandbox run` commands are opaque.

Create mode-`000` sentinel files at:

```text
~/.cargo/config
~/.cargo/config.toml
~/.cargo/credentials
~/.cargo/credentials.toml
```

Assert doctor warns by presence only and never prints sentinel contents.

- [ ] **Step 3: Verify the red state**

Run: `go test -count=1 ./internal/sandboxprofile ./internal/cli -run 'TestResolveReadOnly|TestDoctor.*(Cache|Cargo|Profile|Tool)'`

Expected: FAIL because the resolver and warnings are absent.

- [ ] **Step 4: Implement inspectable-profile and warning helpers**

Retain the first successful `config.LoadLauncher` result in `runDoctor`. Inspect
every `{{self}} sandbox run` command, parsing separate, inline, and omitted
profile references. Resolve that built-in policy with `ResolveReadOnly`, and compare expanded paths using
`filepath.Rel`, never raw string prefixes. Print warning field, entry, impact,
and remediation to `env.Stdout` without incrementing `failures`.

Use `os.Lstat` only for Cargo-file presence. The warning directs users to
project `.cargo/config.toml` plus an explicitly allowed
`CARGO_REGISTRIES_REGISTRY_NAME_TOKEN`, explaining that `REGISTRY_NAME` must
match the registry key in project `.cargo/config.toml`; it never reads or
copies host files.

- [ ] **Step 5: Verify doctor behavior**

Run: `gofmt -w internal/sandboxprofile/resolve.go internal/sandboxprofile/profile_test.go internal/cli/doctor.go internal/cli/doctor_test.go`

Run: `go test -count=1 ./internal/sandboxprofile ./internal/cli`

Expected: PASS.

---

### Task 10: Backend and Deterministic Tool Integration Tests

**Files:**
- Create: `internal/sandboxrun/cache_integration_linux_test.go`
- Create: `internal/sandboxrun/cache_integration_darwin_test.go`
- Create: `internal/e2e/cache_isolation_test.go`
- Create: `internal/e2e/testdata/rust-cache-probe/Cargo.toml`
- Create: `internal/e2e/testdata/rust-cache-probe/src/main.rs`
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 1: Add backend denial tests**

Reuse `runBwrapped`/`requireBwrap` on Linux and `runSandboxed` on macOS. Create
host markers beneath a uniquely named directory under the real user home, not
under `/tmp` or `$TMPDIR` because those are baseline grants. Assert:

```text
host global cache marker: unreadable and unchanged
selected cache leaf: writable
sibling scope leaf: unreadable and unwritable
parent ~/.cache/omac and .locks: not granted
```

- [ ] **Step 2: Run platform-local backend tests**

Run: `go test -count=1 -run 'TestIntegration.*Cache' ./internal/sandboxrun`

Expected before implementation completion: FAIL for any leaked/broad grant;
after Tasks 3-6: PASS on the current platform. Linux must report a skip if
bubblewrap is unavailable locally; CI added below must not skip it.

- [ ] **Step 3: Add deterministic full-CLI cache tests**

Under the `e2e` build tag, build omac with the existing helper and launch a
shell as the inner command without model credentials. Cover:

```go
func TestE2ECachePersistentReuse(t *testing.T)
func TestE2ECacheEphemeralStartsEmptyAndCleansUp(t *testing.T)
func TestE2ECacheEnvironmentOverridesAllowlist(t *testing.T)
func TestE2ECacheMainAndLinkedWorktreesAreIsolated(t *testing.T)
func TestE2ECachePersistentFailureSuggestsEphemeral(t *testing.T)
func TestE2ECacheServeScopes(t *testing.T)
func TestE2ECacheActiveScopeCannotBeCleared(t *testing.T)
func TestE2ECacheTools(t *testing.T)
func TestE2ECacheProvenance(t *testing.T)
```

Tool probes are deterministic and network-free:

```text
go env GOCACHE GOMODCACHE
npm config get cache
python3 -m pip cache dir
cargo build --manifest-path internal/e2e/testdata/rust-cache-probe/Cargo.toml
```

Create the Rust fixture with exactly:

```toml
[package]
name = "omac-cache-probe"
version = "0.1.0"
edition = "2021"
```

```rust
fn main() {
    println!("cache probe");
}
```

Skip an unavailable optional tool locally with an explicit test message, but CI
must install all four. Assert Rustup/toolchain and `~/.cargo/bin` are readable
but not writable. Create all four Cargo config/credential files with distinct
markers, assert their contents are denied, and assert none is copied into the
isolated `CARGO_HOME`.

The linked-worktree test creates a real main/linked pair, writes a marker
through the main worktree's launched cache, launches from the linked worktree,
and proves the marker is inaccessible. For CI-installed tool binaries outside
production baseline paths, add test-only read entries for the resolved binary
directories and required runtime roots: `go env GOROOT`, Python's
`sys.base_prefix`, and npm's installation root derived from `npm root -g`.
Never grant the entire hosted-toolcache parent.

The recovery test places an unsafe symlink or regular file at
`~/.cache/omac`, asserts a persistent launch fails before its inner marker is
written and prints both the failed path and `--ephemeral-cache` hint, then
retries with `--ephemeral-cache` and proves the inner command succeeds without
creating persistent cache state.

`TestE2ECacheProvenance` invokes the built omac binary's `provenance --json`
directly, parses the cache object, and compares its path/mappings with a real
shell launch. It must not call `providerFor`, `runAgent`, or `runAuditAgent`.

Prove writes as well as reported configuration: compile a local Go package and
assert `GOCACHE` gains entries; generate a local file-based Go module proxy,
download one fixture module, and assert it appears in `GOMODCACHE`; run
`npm cache add` against a local package fixture and verify the npm cache; make
pip build/cache a programmatically generated local source archive served from a
loopback HTTP fixture, verify it with `pip cache list`, and remove it with
`pip cache remove omac-cache-probe`; build the Rust fixture and write/read a
probe under `CARGO_HOME`. Host-global markers remain unchanged.

Start the pip fixture before writing the test profile, add its allocated port
to test-only `network.open_port`, and invoke pip with `--no-deps
--no-build-isolation` so the probe cannot fetch external build dependencies.

Run serve shell probes as `omac serve claude-code --inner /bin/sh ...` because
Claude has no `ServerLaunch` subcommand; do not use default OpenCode, which
would inject `serve` into the shell argv.

- [ ] **Step 4: Add a PR-gating cache integration matrix**

Extend `.github/workflows/ci.yml` with Linux setup for bubblewrap/AppArmor and
explicit Node, Python/pip, and Rust setup; macOS uses Seatbelt and the same tool
set. Run:

```yaml
  cache-integration:
    name: Cache integration (${{ matrix.os }})
    runs-on: ${{ matrix.os }}
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu-latest, macos-latest]
    steps:
      - uses: actions/checkout@v4
      - if: runner.os == 'Linux'
        run: |
          sudo apt-get update
          sudo apt-get install -y bubblewrap python3-pip
          sudo tee /etc/apparmor.d/bwrap > /dev/null <<'EOF'
          abi <abi/4.0>,
          /usr/bin/bwrap flags=(unconfined) {
            userns,
          }
          EOF
          sudo apparmor_parser -r /etc/apparmor.d/bwrap
          bwrap --ro-bind / / -- /bin/true
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - uses: actions/setup-node@v4
        with:
          node-version: '24'
      - uses: actions/setup-python@v5
        with:
          python-version: '3.13'
      - uses: dtolnay/rust-toolchain@stable
      - run: go test -count=1 -run '^TestIntegration.*Cache' ./internal/sandboxrun
      - run: go test -tags=e2e -count=1 -timeout=15m -run '^TestE2ECache' ./internal/e2e/
```

This job must not require model or marketplace secrets. Keep the existing
ordinary test job unchanged. The explicit bwrap smoke command ensures a broken
Linux backend fails instead of letting integration tests skip green.

- [ ] **Step 5: Verify deterministic integration tests**

Run on the current platform:

```text
go test -count=1 -run 'TestIntegration.*Cache' ./internal/sandboxrun
go test -tags=e2e -count=1 -timeout=15m -run '^TestE2ECache' ./internal/e2e/
```

Expected: PASS for installed prerequisites; explicit skips only where the local
host lacks a tool/backend that CI installs.

---

### Task 11: Existing E2E Harnesses and Fixtures

**Files:**
- Modify: `internal/e2e/e2e_test.go`
- Modify: `internal/e2e/allowance.go`
- Modify: `internal/e2e/harnesses.go`
- Modify: `internal/e2e/harnesses_test.go`
- Modify: `internal/e2e/provenance_test.go`
- Modify: `.opencode/skills/self-audit/scripts/audit.sh`
- Modify: `Dockerfile.e2e`
- Modify: `scripts/e2e-docker.sh`

- [ ] **Step 1: Make E2E fixtures use production-safe grants**

Replace `writeSandboxProfile`'s manually copied broad grants with a profile
derived from `sandboxprofile.DefaultProfile()`, changing only test network and
`environment.allow_vars` fields for ordinary harness lifecycle tests. The
deterministic tool test may additionally add only the resolved binary
directories described in Task 10. Remove `.cache/opencode` setup assumptions
and update comments to describe `XDG_CACHE_HOME`.

Update Darwin harness tests so they expect codex to be excluded on macOS and
included on Linux. Extend provenance E2E JSON assertions with the cache section.

- [ ] **Step 2: Extend the self-audit cache assertions**

Have the audit print `OMAC_CACHE_DIR`, `OMAC_CACHE_MODE`, and all tool mappings;
write a marker to the selected cache; and prove host-global cache markers are
denied. Keep the `allow_vars` fixture intentionally unaware of non-`OMAC_*`
cache names so the test exercises Task 3's trusted re-injection.

- [ ] **Step 3: Update Docker support**

Install `python3-pip`, `build-essential`, and rustup's minimal stable toolchain
in `Dockerfile.e2e`, and add `/root/.cargo/bin` to `PATH`. Add a `cache`
subcommand to `scripts/e2e-docker.sh` that executes:

```text
go test -tags=e2e -timeout=15m -v -run '^TestE2ECache' ./internal/e2e/
```

The Docker `cache` command treats missing Go, node/npm, Python/pip, Cargo, or
bubblewrap as a failure rather than a skip.

- [ ] **Step 4: Run fixture and Docker verification**

Run: `go test -tags=e2e -count=1 -run 'Test(AllHarnesses|HarnessByName|E2ECacheProvenance)' ./internal/e2e/`

Expected: PASS without model calls. Keep the existing agent-backed
`TestE2EProvenance` in the credentialed scheduled E2E run.

If Docker is available, run: `scripts/e2e-docker.sh cache`

Expected: PASS on Linux/bubblewrap. Docker does not cover macOS Seatbelt.

- [ ] **Step 5: Run all-harness lifecycle checks where credentials exist**

Use the existing commands from `AGENTS.md` or the scheduled workflow for
OpenCode, Claude Code, Codex on Linux, and Copilot. A harness that requires a
writable cache must gain a documented redirect under `OMAC_CACHE_DIR`; do not
restore its host-global cache grant.

---

### Task 12: User and Normative Documentation

**Files:**
- Modify: `README.md`
- Modify: `oh-my-agentic-coder.md`
- Modify: `docs/MULTI_DIR_DESKTOP.md`
- Modify: `docs/NONO_SANDBOX.md`
- Modify: `openspec/changes/native-sandbox/specs/sandbox-launch/spec.md`
- Modify: `internal/config/launcher.go`
- Modify: `AGENTS.md`

- [ ] **Step 1: Update the documented filesystem boundary**

Replace claims that all of `~/.cache`, `~/Library/Caches`, `~/go`,
`~/.cargo`, and `~/.rustup` are read-write. Document the selected cache leaf,
read-only tool paths, hardcoded-cache denial, and existing-profile doctor
warnings.

- [ ] **Step 2: Document modes, trust domains, and cleanup**

Document persistent default behavior, accepted same-domain poisoning,
`--ephemeral-cache`, main/linked worktree path identity, single-directory
serve, shared multi-directory serve cache, `--no-sandbox`, and:

```text
omac cache clear
omac cache clear --all
```

Explain active-scope refusal/skipping and private Cargo registry setup.

- [ ] **Step 3: Update CLI/provenance and sandbox implementation docs**

Add `OMAC_CACHE_DIR`, `OMAC_CACHE_MODE`, all six tool redirects, verbose output,
cache provenance, and external nono-profile limitations. Correct
`docs/NONO_SANDBOX.md` if it still calls nono the compiled default. Update the
OpenSpec requirement so it no longer normatively requires broad cache/tool-home
grants. Update `internal/config/launcher.go` comments that claim equivalence to
the old broad nono profile, and document the new Docker `cache` command in
`AGENTS.md`.

- [ ] **Step 4: Check documentation consistency**

Run content searches for stale normative claims:

```text
~/.cache
~/Library/Caches
~/go
~/.cargo
~/.rustup
```

Historical design documents may retain historical context, but README, active
OpenSpec requirements, current implementation docs, E2E comments, and CLI help
must describe the new behavior.

Run: `git diff --check`

Expected: no whitespace errors.

---

### Task 13: Final Verification and MR Notes

**Files:**
- Verify all files above
- Do not create an MR in this task unless the user explicitly requests it

- [ ] **Step 1: Run formatting and static checks**

Run:

```bash
go_files=$(git ls-files --cached --others --exclude-standard '*.go')
unformatted=$(gofmt -l $go_files)
test -z "$unformatted" || { printf 'unformatted Go files:\n%s\n' "$unformatted"; exit 1; }
go vet ./...
go build -v ./...
```

Expected: all commands exit 0.

- [ ] **Step 2: Run the full race suite**

Run: `go test -race -count=1 -timeout=5m ./...`

Expected: PASS with zero failures. Record any platform-specific skipped kernel
test explicitly; do not report it as cross-platform evidence.

- [ ] **Step 3: Run deterministic cache E2E**

Run: `go test -tags=e2e -count=1 -timeout=15m -run '^TestE2ECache' ./internal/e2e/`

Expected: PASS on the current platform for installed prerequisites. CI must
provide final Linux and macOS evidence.

- [ ] **Step 4: Review the diff against every acceptance criterion**

Confirm explicitly:

```text
no broad new-profile cache/tool-home grants
existing profiles unchanged and warned about
persistent and ephemeral scopes correctly granted
main and linked worktrees use one path-based implementation
multi-directory serve uses one documented serve scope
allow_vars cannot strip validated cache variables
hardcoded caches fail closed and are diagnosable
private Cargo credentials are not exposed
active caches cannot be cleared
manual clear current/all works
Linux and macOS coverage is present
```

- [ ] **Step 5: Preserve required MR wording for later use**

When an MR is requested, its description must state:

- Persistent mode isolates host applications and independent omac trust
  domains, but does not prevent poisoning between sessions in one domain.
- `--ephemeral-cache` is the stronger opt-in boundary with cold-start cost.
- Main and linked worktrees differ because canonical paths differ, not because
  of Git metadata.
- Multi-directory serve shares one cache and is not per-project isolated.
- Existing profiles are not rewritten; doctor only advises.
- Host Cargo credentials are not copied; private registries need project-local
  configuration and an explicit token.
- `--no-sandbox` has no cache-isolation guarantee.

Do not claim the work is complete until fresh verification output supports each
applicable statement.
