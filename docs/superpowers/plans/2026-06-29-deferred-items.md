# Deferred Items Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the three deferred items from the codex+copilot change: `omac manifest` subcommand, pre-flight binary check, and `*_HOME` env overrides for all 4 harnesses.

**Architecture:** Three independent features, each TDD. Item 1 adds a new CLI subcommand wrapping `manifest.Render()`. Item 2 adds a shared `checkInnerBinary` helper called from `runLaunch` and `runServe`, plus an advisory section in `omac doctor`. Item 3 adds a `HomeEnv` field and exported `ConfigHome()` method to the `Harness` struct, wired into `GlobalSkillsDir()`, `GlobalBridgeDir()`, and session path resolvers.

**Tech Stack:** Go, stdlib only (`os/exec`, `os`, `flag`, `io`)

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `internal/cli/manifest_cmd.go` | Create | `omac manifest` subcommand: parse flags, read JSON, call `manifest.Render()` |
| `internal/cli/manifest_cmd_test.go` | Create | Tests for manifest subcommand |
| `internal/cli/cli.go` | Modify | Register `manifest` command + usage text |
| `internal/cli/start.go` | Modify | Add `checkInnerBinary()` helper + call in `runLaunch` |
| `internal/cli/serve.go` | Modify | Call `checkInnerBinary()` in `runServe` |
| `internal/cli/doctor.go` | Modify | Add harness binary status section |
| `internal/cli/binary_check_test.go` | Create | Tests for binary check |
| `internal/config/harness.go` | Modify | Add `HomeEnv` field + `ConfigHome()` method; update `GlobalSkillsDir()` + `GlobalBridgeDir()` |
| `internal/config/harness_test.go` | Modify | Tests for `ConfigHome()` + env override |
| `internal/session/session.go` | Modify | Update path resolvers to take `config.Harness` param |
| `internal/session/session_test.go` | Modify | Tests for env override in session listing |

---

### Task 1: `omac manifest` subcommand

**Files:**
- Create: `internal/cli/manifest_cmd.go`
- Create: `internal/cli/manifest_cmd_test.go`
- Modify: `internal/cli/cli.go`

- [ ] **Step 1: Write failing tests**

Create `internal/cli/manifest_cmd_test.go`:

```go
package cli

import (
	"bytes"
	"flag"
	"os"
	"testing"
)

func TestRunManifestStdin(t *testing.T) {
	activateJSON := `{"dir":"/proj","state":"active","skills":[{"name":"echo","scope":"workdir","state":"ready","base":"http://127.0.0.1:9000/echo"}]}`
	stdin := bytes.NewBufferString(activateJSON)
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	env := &Env{Stdin: stdinFile(stdin), Stdout: out, Stderr: errOut, Version: "test", Workdir: "."}

	code := runManifest([]string{"--skills-dir", ".claude/skills"}, env)
	if code != ExitOK {
		t.Fatalf("code=%d, want %d; stderr=%s", code, ExitOK, errOut.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("echo")) {
		t.Errorf("output missing skill name; got:\n%s", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(".claude/skills")) {
		t.Errorf("output missing skills dir; got:\n%s", out.String())
	}
}

func TestRunManifestInputFile(t *testing.T) {
	activateJSON := `{"dir":"/proj","state":"active","skills":[]}`
	tmp := t.TempDir()
	f := filepathJoin(t, tmp, "activate.json")
	mustWrite(t, f, activateJSON)
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	env := &Env{Stdin: devNull(t), Stdout: out, Stderr: errOut, Version: "test", Workdir: "."}

	code := runManifest([]string{"--skills-dir", ".codex/skills", "--input", f}, env)
	if code != ExitOK {
		t.Fatalf("code=%d, want %d; stderr=%s", code, ExitOK, errOut.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("/proj")) {
		t.Errorf("output missing dir; got:\n%s", out.String())
	}
}

func TestRunManifestMissingSkillsDir(t *testing.T) {
	env := &Env{Stdin: devNull(t), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Version: "test", Workdir: "."}
	code := runManifest([]string{}, env)
	if code != ExitMisuse {
		t.Fatalf("code=%d, want ExitMisuse(%d)", code, ExitMisuse)
	}
}

func TestRunManifestInputFileNotFound(t *testing.T) {
	env := &Env{Stdin: devNull(t), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Version: "test", Workdir: "."}
	code := runManifest([]string{"--skills-dir", ".claude/skills", "--input", "/nonexistent-xyz.json"}, env)
	if code == ExitOK {
		t.Fatal("expected non-zero exit for missing input file")
	}
}

func TestRunManifestInvalidJSON(t *testing.T) {
	stdin := bytes.NewBufferString("{not valid json")
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	env := &Env{Stdin: stdinFile(stdin), Stdout: out, Stderr: errOut, Version: "test", Workdir: "."}

	code := runManifest([]string{"--skills-dir", ".claude/skills"}, env)
	if code != ExitOK {
		t.Fatalf("code=%d, want ExitOK (Render returns \"\" on parse error)", code)
	}
	if out.Len() != 0 {
		t.Errorf("expected empty output for invalid JSON; got:\n%s", out.String())
	}
}

// --- helpers ---

func stdinFile(b *bytes.Buffer) *os.File {
	r, w, _ := os.Pipe()
	w.Write(b.Bytes())
	w.Close()
	return r
}

func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func filepathJoin(t *testing.T, dir, name string) string {
	t.Helper()
	return dir + "/" + name
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// Suppress unused import if flag is only used in step 3.
var _ = flag.NewFlagSet
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run TestRunManifest -v`
Expected: FAIL — `runManifest` undefined.

- [ ] **Step 3: Implement `runManifest`**

Create `internal/cli/manifest_cmd.go`:

```go
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/tngtech/oh-my-agentic-coder/internal/manifest"
)

// runManifest renders the skills manifest from activate-response JSON.
// It reads JSON from --input <file> or stdin, and writes the rendered
// markdown to stdout.
func runManifest(args []string, env *Env) int {
	fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	skillsDir := fs.String("skills-dir", "", "active harness skills dir (required)")
	input := fs.String("input", "", "activate-response JSON file (default: stdin)")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac manifest --skills-dir <dir> [--input <file>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}
	if *skillsDir == "" {
		fmt.Fprintln(env.Stderr, "omac manifest: --skills-dir is required")
		return ExitMisuse
	}

	var data []byte
	var err error
	if *input != "" {
		data, err = os.ReadFile(*input)
		if err != nil {
			fmt.Fprintf(env.Stderr, "omac manifest: read %s: %v\n", *input, err)
			return ExitIOError
		}
	} else {
		data, err = io.ReadAll(env.Stdin)
		if err != nil {
			fmt.Fprintf(env.Stderr, "omac manifest: read stdin: %v\n", err)
			return ExitIOError
		}
	}

	out := manifest.Render(string(data), *skillsDir)
	if out == "" && len(data) > 0 {
		fmt.Fprintln(env.Stderr, "omac manifest: warning: failed to parse activate-response JSON")
	}
	fmt.Fprint(env.Stdout, out)
	return ExitOK
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestRunManifest -v`
Expected: PASS — all 5 tests.

- [ ] **Step 5: Register command in cli.go**

In `internal/cli/cli.go`, add to the `commands()` map (after the `"version"` entry):

```go
"manifest": {Name: "manifest", Short: "Render the skills manifest from activate-response JSON.", Run: runManifest},
```

In `printUsage()`, add after the `version` line:

```
  manifest    Render the skills manifest from activate-response JSON.
```

- [ ] **Step 6: Verify build + commit**

```bash
go build ./...
go test ./internal/cli/ -run TestRunManifest -v
git add internal/cli/manifest_cmd.go internal/cli/manifest_cmd_test.go internal/cli/cli.go
git commit -m "feat: add omac manifest subcommand wrapping manifest.Render()"
```

---

### Task 2: Pre-flight binary check in `runLaunch`

**Files:**
- Modify: `internal/cli/start.go`
- Create: `internal/cli/binary_check_test.go`

- [ ] **Step 1: Write failing test**

Create `internal/cli/binary_check_test.go`:

```go
package cli

import (
	"bytes"
	"os"
	"testing"
)

func TestCheckInnerBinaryFound(t *testing.T) {
	// /bin/echo or /usr/bin/echo — guaranteed to exist
	env := &Env{Stderr: &bytes.Buffer{}, Stdout: &bytes.Buffer{}, Stdin: devNull(t), Version: "test", Workdir: "."}
	// Use a harness whose InnerCmd is "echo" — guaranteed on PATH
	h := testHarnessWithInnerCmd("echo")
	if code := checkInnerBinary(h, "test", env); code != ExitOK {
		t.Errorf("checkInnerBinary(echo) = %d, want ExitOK(%d)", code, ExitOK)
	}
}

func TestCheckInnerBinaryMissing(t *testing.T) {
	errOut := &bytes.Buffer{}
	env := &Env{Stderr: errOut, Stdout: &bytes.Buffer{}, Stdin: devNull(t), Version: "test", Workdir: "."}
	h := testHarnessWithInnerCmd("nonexistent-binary-xyz-12345")
	code := checkInnerBinary(h, "test", env)
	if code != ExitPrerequisiteMissing {
		t.Errorf("checkInnerBinary(nonexistent) = %d, want ExitPrerequisiteMissing(%d)", code, ExitPrerequisiteMissing)
	}
	if !bytes.Contains(errOut.Bytes(), []byte("not found on $PATH")) {
		t.Errorf("stderr missing 'not found' message; got:\n%s", errOut.String())
	}
}

func TestCheckInnerBinaryEmptyCmd(t *testing.T) {
	env := &Env{Stderr: &bytes.Buffer{}, Stdout: &bytes.Buffer{}, Stdin: devNull(t), Version: "test", Workdir: "."}
	h := testHarnessWithInnerCmd("") // empty InnerCmd
	if code := checkInnerBinary(h, "test", env); code != ExitOK {
		t.Errorf("checkInnerBinary(empty) = %d, want ExitOK (skip)", code)
	}
}

// testHarnessWithInnerCmd builds a minimal Harness with the given InnerCmd[0].
func testHarnessWithInnerCmd(bin string) config.Harness {
	cmd := []string{}
	if bin != "" {
		cmd = []string{bin}
	}
	return config.Harness{Name: "test", InnerCmd: cmd}
}
```

Note: This test file imports `config`, so add the import. The test file will fail to compile until `checkInnerBinary` exists.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestCheckInnerBinary -v`
Expected: FAIL — `checkInnerBinary` undefined.

- [ ] **Step 3: Implement `checkInnerBinary`**

In `internal/cli/start.go`, add (near `runLaunch`, before the function):

```go
// checkInnerBinary verifies the harness's inner command binary is on $PATH.
// Returns ExitOK when found, ExitPrerequisiteMissing when missing, ExitOK
// when InnerCmd is empty (defensive skip). Called by runLaunch and runServe.
func checkInnerBinary(harness config.Harness, prefix string, env *Env) int {
	if len(harness.InnerCmd) == 0 {
		return ExitOK
	}
	if _, err := exec.LookPath(harness.InnerCmd[0]); err != nil {
		fmt.Fprintf(env.Stderr, "%s: harness binary %q not found on $PATH; install it or pass --inner-cmd <path>\n", prefix, harness.InnerCmd[0])
		return ExitPrerequisiteMissing
	}
	return ExitOK
}
```

Ensure `os/exec` is imported in `start.go` (it likely already is — check).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestCheckInnerBinary -v`
Expected: PASS — all 3 tests.

- [ ] **Step 5: Wire into `runLaunch`**

In `internal/cli/start.go`, in `runLaunch()`, after config load (after the `prof, ok := lc.Sandbox.Profiles[profName]` block, before the `// 2. Reconcile registry` comment), add:

```go
// 1b. Pre-flight: inner harness binary must be on $PATH.
if innerCmdOverride == "" {
    if code := checkInnerBinary(harness, prefix, env); code != ExitOK {
        return code
    }
}
```

- [ ] **Step 6: Verify build + commit**

```bash
go build ./...
go test ./internal/cli/ -run "TestCheckInnerBinary|TestRunLaunch" -v
git add internal/cli/start.go internal/cli/binary_check_test.go
git commit -m "feat: pre-flight binary check in runLaunch via checkInnerBinary"
```

---

### Task 3: Pre-flight binary check in `runServe`

**Files:**
- Modify: `internal/cli/serve.go`

- [ ] **Step 1: Write failing test**

Add to `internal/cli/binary_check_test.go`:

```go
func TestCheckInnerBinaryInServe(t *testing.T) {
	// Integration-style: verify runServe fails fast with missing binary.
	// We can't easily call runServe (it starts servers), so we test
	// checkInnerBinary is called by checking that a missing binary
	// returns ExitPrerequisiteMissing via the shared helper.
	errOut := &bytes.Buffer{}
	env := &Env{Stderr: errOut, Stdout: &bytes.Buffer{}, Stdin: devNull(t), Version: "test", Workdir: "."}
	h := testHarnessWithInnerCmd("nonexistent-binary-xyz-serve-999")
	code := checkInnerBinary(h, "omac serve", env)
	if code != ExitPrerequisiteMissing {
		t.Errorf("checkInnerBinary for serve = %d, want ExitPrerequisiteMissing(%d)", code, ExitPrerequisiteMissing)
	}
	if !bytes.Contains(errOut.Bytes(), []byte("omac serve")) {
		t.Errorf("stderr missing 'omac serve' prefix; got:\n%s", errOut.String())
	}
}
```

- [ ] **Step 2: Run test to verify it passes** (already passes — `checkInnerBinary` exists from Task 2)

Run: `go test ./internal/cli/ -run TestCheckInnerBinaryInServe -v`
Expected: PASS.

- [ ] **Step 3: Wire into `runServe`**

In `internal/cli/serve.go`, in `runServe()`, after the sandbox profile resolution block (after `prof, profOK := lc.Sandbox.Profiles[profName]` and its error check, before `// Normalize pre-declared roots`), add:

```go
// Pre-flight: inner harness binary must be on $PATH (unless --no-inner
// or --inner override).
if !*noInner && *innerCmdOverride == "" {
    if code := checkInnerBinary(harness, "omac serve", env); code != ExitOK {
        return code
    }
}
```

- [ ] **Step 4: Verify build + commit**

```bash
go build ./...
go test ./internal/cli/ -run TestCheckInnerBinary -v
git add internal/cli/serve.go internal/cli/binary_check_test.go
git commit -m "feat: pre-flight binary check in runServe"
```

---

### Task 4: Harness binary status in `omac doctor`

**Files:**
- Modify: `internal/cli/doctor.go`

- [ ] **Step 1: Write failing test**

Add to `internal/cli/binary_check_test.go`:

```go
func TestDoctorHarnessBinarySection(t *testing.T) {
	dir := t.TempDir()
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	env := &Env{Version: "test", Workdir: dir, Stdout: out, Stderr: errOut, Stdin: devNull(t)}

	_ = runDoctor([]string{}, env)
	output := out.String()
	if !bytes.Contains([]byte(output), []byte("Inner harnesses")) {
		t.Errorf("doctor output missing 'Inner harnesses' section; got:\n%s", output)
	}
	// At least one harness should appear
	for _, name := range []string{"opencode", "claude", "codex", "copilot"} {
		if !bytes.Contains([]byte(output), []byte(name)) {
			t.Errorf("doctor output missing harness %q; got:\n%s", name, output)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestDoctorHarnessBinarySection -v`
Expected: FAIL — "Inner harnesses" not in output.

- [ ] **Step 3: Implement doctor section**

In `internal/cli/doctor.go`, add a new function and call it before `doctorBuiltinSkills(env)` (around line 119):

```go
// doctorHarnessBinaries reports which harness binaries are on $PATH.
// Advisory only — does not affect the exit code.
func doctorHarnessBinaries(env *Env) {
	fmt.Fprintln(env.Stdout, "Inner harnesses:")
	for _, h := range config.AllHarnesses() {
		if len(h.InnerCmd) == 0 {
			continue
		}
		bin := h.InnerCmd[0]
		if _, err := exec.LookPath(bin); err == nil {
			fmt.Fprintf(env.Stdout, "  [ok]   %-12s binary=%s found\n", h.Name, bin)
		} else {
			fmt.Fprintf(env.Stdout, "  [warn] %-12s binary=%s not on $PATH\n", h.Name, bin)
		}
	}
}
```

Add the call in `runDoctor`, before `doctorBuiltinSkills(env)`:

```go
// Inner harness binary status.
doctorHarnessBinaries(env)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestDoctorHarnessBinarySection -v`
Expected: PASS.

- [ ] **Step 5: Verify build + commit**

```bash
go build ./...
go test ./internal/cli/ -run "TestDoctor" -v
git add internal/cli/doctor.go internal/cli/binary_check_test.go
git commit -m "feat: harness binary status section in omac doctor"
```

---

### Task 5: `HomeEnv` field + `ConfigHome()` method

**Files:**
- Modify: `internal/config/harness.go`
- Modify: `internal/config/harness_test.go`

- [ ] **Step 1: Write failing tests**

Add to `internal/config/harness_test.go`:

```go
func TestConfigHomeEnvOverride(t *testing.T) {
	h, _ := LookupHarness("codex")
	t.Setenv("CODEX_HOME", "/tmp/codex-home")
	if got := h.ConfigHome(); got != "/tmp/codex-home" {
		t.Errorf("ConfigHome() = %q, want /tmp/codex-home", got)
	}
}

func TestConfigHomeEnvOverrideUnset(t *testing.T) {
	h, _ := LookupHarness("codex")
	t.Setenv("CODEX_HOME", "")
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".codex")
	if got := h.ConfigHome(); got != want {
		t.Errorf("ConfigHome() = %q, want %q", got, want)
	}
}

func TestConfigHomeEnvOverrideClaude(t *testing.T) {
	h, _ := LookupHarness("claude-code")
	t.Setenv("CLAUDE_HOME", "/tmp/claude-home")
	if got := h.ConfigHome(); got != "/tmp/claude-home" {
		t.Errorf("ConfigHome() = %q, want /tmp/claude-home", got)
	}
}

func TestConfigHomeEnvOverrideOpenCode(t *testing.T) {
	h, _ := LookupHarness("opencode")
	t.Setenv("OPENCODE_HOME", "/tmp/oc-home")
	if got := h.ConfigHome(); got != "/tmp/oc-home" {
		t.Errorf("ConfigHome() = %q, want /tmp/oc-home", got)
	}
}

func TestGlobalSkillsDirEnvOverride(t *testing.T) {
	h, _ := LookupHarness("codex")
	t.Setenv("CODEX_HOME", "/tmp/codex-skills")
	want := "/tmp/codex-skills/skills"
	if got := h.GlobalSkillsDir(); got != want {
		t.Errorf("GlobalSkillsDir() = %q, want %q", got, want)
	}
}

func TestGlobalSkillsDirEnvOverrideClaude(t *testing.T) {
	h, _ := LookupHarness("claude-code")
	t.Setenv("CLAUDE_HOME", "/tmp/claude-skills")
	want := "/tmp/claude-skills/skills"
	if got := h.GlobalSkillsDir(); got != want {
		t.Errorf("GlobalSkillsDir() = %q, want %q", got, want)
	}
}

func TestGlobalSkillsDirEnvOverrideOpenCode(t *testing.T) {
	h, _ := LookupHarness("opencode")
	t.Setenv("OPENCODE_HOME", "/tmp/oc-skills")
	want := "/tmp/oc-skills/skills"
	if got := h.GlobalSkillsDir(); got != want {
		t.Errorf("GlobalSkillsDir() = %q, want %q", got, want)
	}
}
```

Ensure `os` and `path/filepath` are imported in the test file.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run "TestConfigHome|TestGlobalSkillsDirEnv" -v`
Expected: FAIL — `ConfigHome` undefined, `HomeEnv` field missing.

- [ ] **Step 3: Add `HomeEnv` field to `Harness` struct**

In `internal/config/harness.go`, add after the `UserConfigHome` field (after line 64):

```go
// HomeEnv, when non-empty, names an environment variable whose value
// replaces the harness's full config home directory. When the env var
// is unset or empty, the harness falls back to its default config home
// (UserConfigHome under $HOME, or XDG for opencode).
HomeEnv string
```

- [ ] **Step 4: Set `HomeEnv` in registry**

In `harnessRegistry()`, add `HomeEnv` to each harness descriptor:

```go
// opencode (after UserConfigHome or SkillsBase):
HomeEnv: "OPENCODE_HOME",

// claude-code:
HomeEnv: "CLAUDE_HOME",

// codex:
HomeEnv: "CODEX_HOME",

// copilot:
HomeEnv: "COPILOT_HOME",
```

- [ ] **Step 5: Implement `ConfigHome()` method**

In `internal/config/harness.go`, add:

```go
// ConfigHome returns the harness's full config home directory, honoring
// the HomeEnv override. For UserConfigHome harnesses (claude, codex,
// copilot), this is $HOME/<UserConfigHome> by default. For XDG harnesses
// (opencode), this is $XDG_CONFIG_HOME/<base> or ~/.config/<base> by
// default. When HomeEnv is set and non-empty, its value replaces the
// default entirely. Returns "" when no home can be resolved.
func (h Harness) ConfigHome() string {
	if h.HomeEnv != "" {
		if dir := os.Getenv(h.HomeEnv); dir != "" {
			return dir
		}
	}
	base := h.SkillsBase
	if base == "" {
		base = SharedSkillsBase
	}
	if h.UserConfigHome != "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		return filepath.Join(home, h.UserConfigHome)
	}
	root := userConfigRoot()
	if root == "" {
		return ""
	}
	return filepath.Join(root, base)
}
```

- [ ] **Step 6: Update `GlobalSkillsDir()` to use `ConfigHome()`**

Replace the existing `GlobalSkillsDir()` body with:

```go
func (h Harness) GlobalSkillsDir() string {
	home := h.ConfigHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "skills")
}
```

- [ ] **Step 7: Update `GlobalBridgeDir()` to use `ConfigHome()`**

In `GlobalBridgeDir()`, replace `root := userConfigRoot()` and the `root == ""` check with:

```go
home := h.ConfigHome()
if home == "" {
	return ""
}
return filepath.Join(home, leaf)
```

(Remove the old `root := userConfigRoot()` line and `if root == "" { return "" }`.)

- [ ] **Step 8: Run tests to verify they pass**

Run: `go test ./internal/config/ -run "TestConfigHome|TestGlobalSkillsDirEnv" -v`
Expected: PASS — all 7 tests.

- [ ] **Step 9: Run full config test suite**

Run: `go test ./internal/config/ -v`
Expected: PASS — no regressions.

- [ ] **Step 10: Commit**

```bash
git add internal/config/harness.go internal/config/harness_test.go
git commit -m "feat: add HomeEnv field + ConfigHome() method for all 4 harnesses"
```

---

### Task 6: Session path resolvers with env override

**Files:**
- Modify: `internal/session/session.go`
- Modify: `internal/session/session_test.go`

- [ ] **Step 1: Write failing tests**

Add to `internal/session/session_test.go`:

```go
func TestClaudeProjectsRootEnvOverride(t *testing.T) {
	h, _ := config.LookupHarness("claude-code")
	t.Setenv("CLAUDE_HOME", "/tmp/claude-test-home")
	got := claudeProjectsRoot(h)
	if got != "/tmp/claude-test-home/projects" {
		t.Errorf("claudeProjectsRoot = %q, want /tmp/claude-test-home/projects", got)
	}
}

func TestCodexSessionsRootEnvOverride(t *testing.T) {
	h, _ := config.LookupHarness("codex")
	t.Setenv("CODEX_HOME", "/tmp/codex-test-home")
	got := codexSessionsRoot(h)
	if got != "/tmp/codex-test-home/sessions" {
		t.Errorf("codexSessionsRoot = %q, want /tmp/codex-test-home/sessions", got)
	}
}

func TestCopilotDBPathEnvOverride(t *testing.T) {
	h, _ := config.LookupHarness("copilot")
	t.Setenv("COPILOT_HOME", "/tmp/copilot-test-home")
	got := copilotDBPath(h)
	if got != "/tmp/copilot-test-home/session-store.db" {
		t.Errorf("copilotDBPath = %q, want /tmp/copilot-test-home/session-store.db", got)
	}
}

func TestSessionListEnvOverrideCopilot(t *testing.T) {
	h, _ := config.LookupHarness("copilot")
	tmpHome := t.TempDir()
	t.Setenv("COPILOT_HOME", tmpHome)
	// Create a fake session-store.db with a minimal schema
	dbPath := tmpHome + "/session-store.db"
	createFakeCopilotDB(t, dbPath)
	sessions := listCopilot("/fake/workdir", copilotDBPath(h))
	// Best-effort: may return nil if schema doesn't match, but should
	// at least find the db file (not return nil due to missing path).
	_ = sessions // smoke test — no panic, path resolved correctly
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/session/ -run "TestClaude.*Env|TestCodex.*Env|TestCopilot.*Env|TestSessionListEnv" -v`
Expected: FAIL — resolvers don't take `config.Harness` param yet.

- [ ] **Step 3: Update path resolvers**

In `internal/session/session.go`, change the three resolver signatures and bodies:

```go
func claudeProjectsRoot(h config.Harness) string {
	home := h.ConfigHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "projects")
}

func codexSessionsRoot(h config.Harness) string {
	home := h.ConfigHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "sessions")
}

func copilotDBPath(h config.Harness) string {
	home := h.ConfigHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "session-store.db")
}
```

- [ ] **Step 4: Update `List()` to pass harness to resolvers**

In `internal/session/session.go`, change `List()`:

```go
func List(h config.Harness, workdir string) ([]Session, error) {
	return list(h, workdir, execRunner, claudeProjectsRoot(h), opencodeDBPath(), codexSessionsRoot(h), copilotDBPath(h))
}
```

(`opencodeDBPath()` stays unchanged — no env override for the data dir.)

- [ ] **Step 5: Fix any test helpers that call the resolvers directly**

Check `internal/session/session_test.go` for existing calls to `claudeProjectsRoot()`, `codexSessionsRoot()`, `copilotDBPath()` without args — they need a `config.Harness` arg now. Update them.

Run: `go build ./internal/session/` to find compile errors, then fix each call site.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/session/ -v`
Expected: PASS — all tests including new env override tests.

- [ ] **Step 7: Commit**

```bash
git add internal/session/session.go internal/session/session_test.go
git commit -m "feat: session path resolvers honor *_HOME env overrides via ConfigHome()"
```

---

### Task 7: Update openspec proposal + final verification

**Files:**
- Modify: `openspec/changes/support-codex-copilot-harnesses/tasks.md`

- [ ] **Step 1: Update tasks.md**

Mark items 7.3, 7.4, 7.5 as complete (they're now implemented):

```markdown
- [x] 7.3 `omac manifest` subcommand
- [x] 7.4 `COPILOT_HOME` env override (expanded to all 4 harnesses)
- [x] 7.5 Pre-flight binary check (`exec.LookPath`)
```

- [ ] **Step 2: Run full test suite**

```bash
go build ./...
go test ./...
```

Expected: all pass, 0 failures.

- [ ] **Step 3: Commit**

```bash
git add openspec/changes/support-codex-copilot-harnesses/tasks.md
git commit -m "docs: mark deferred items as complete in openspec tasks"
```

---

## Self-Review

**1. Spec coverage:**
- Section 1 (manifest subcommand): Task 1 — ✓
- Section 2a (blocking in runLaunch): Task 2 — ✓
- Section 2b (blocking in runServe): Task 3 — ✓
- Section 2c (advisory in doctor): Task 4 — ✓
- Section 3 (HomeEnv + ConfigHome): Task 5 — ✓
- Section 3 (session path resolvers): Task 6 — ✓
- Openspec reconciliation: Task 7 — ✓

**2. Placeholder scan:** No TBD/TODO. All steps have concrete code.

**3. Type consistency:**
- `ConfigHome()` returns `string` — consistent across all call sites.
- `checkInnerBinary(harness config.Harness, prefix string, env *Env) int` — consistent in Tasks 2, 3.
- `claudeProjectsRoot(h config.Harness)`, `codexSessionsRoot(h config.Harness)`, `copilotDBPath(h config.Harness)` — consistent in Task 6.
- `HomeEnv string` field — consistent in Task 5.
