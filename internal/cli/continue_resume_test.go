package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/session"
	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

// devnullEnv builds an Env whose streams all point at /dev/null. Suitable for
// the opts-building helpers, which never read stdin and only write diagnostics.
func devnullEnv(t *testing.T) *Env {
	t.Helper()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() { null.Close() })
	return &Env{Version: "test", Workdir: "/w", Stdout: null, Stderr: null, Stdin: null}
}

func TestBuildContinueOpts(t *testing.T) {
	cases := []struct {
		name          string
		args          []string
		wantHarness   string
		wantInnerArgs []string
		wantVerbose   bool
		wantNoSandbox bool
		wantEphemeral bool
	}{
		{"default harness", nil, "opencode", []string{"--continue"}, false, false, false},
		{"claude token", []string{"claude"}, "claude-code", []string{"--continue"}, false, false, false},
		{"start flags preserved", []string{"--verbose", "--no-sandbox"}, "opencode", []string{"--continue"}, true, true, false},
		{"trailing inner args preserved", []string{"--", "--model", "anthropic/x"}, "opencode", []string{"--continue", "--model", "anthropic/x"}, false, false, false},
		{"claude with flags and inner", []string{"claude", "--verbose", "--", "--foo"}, "claude-code", []string{"--continue", "--foo"}, true, false, false},
		{"session id flag (-s)", []string{"-s", "ses_X"}, "opencode", []string{"--session", "ses_X"}, false, false, false},
		{"session id flag (--session)", []string{"--session", "ses_Y"}, "opencode", []string{"--session", "ses_Y"}, false, false, false},
		{"claude session id", []string{"claude", "-s", "uuid-9"}, "claude-code", []string{"--resume", "uuid-9"}, false, false, false},
		{"session id with inner args", []string{"-s", "ses_Z", "--", "--model", "x"}, "opencode", []string{"--session", "ses_Z", "--model", "x"}, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts, code := buildContinueOpts(c.args, devnullEnv(t))
			if code != ExitOK {
				t.Fatalf("code = %d, want ExitOK", code)
			}
			if opts.harness.Name != c.wantHarness {
				t.Errorf("harness = %q, want %q", opts.harness.Name, c.wantHarness)
			}
			if !reflect.DeepEqual(opts.innerArgs, c.wantInnerArgs) {
				t.Errorf("innerArgs = %v, want %v", opts.innerArgs, c.wantInnerArgs)
			}
			if opts.verbose != c.wantVerbose {
				t.Errorf("verbose = %v, want %v", opts.verbose, c.wantVerbose)
			}
			if opts.noSandbox != c.wantNoSandbox {
				t.Errorf("noSandbox = %v, want %v", opts.noSandbox, c.wantNoSandbox)
			}
			if opts.ephemeralCache != c.wantEphemeral {
				t.Errorf("ephemeralCache = %v, want %v", opts.ephemeralCache, c.wantEphemeral)
			}
		})
	}
}

func TestParseLaunchArgsEphemeralCache(t *testing.T) {
	opts, ok := parseLaunchArgs("start", []string{"--ephemeral-cache"}, devnullEnv(t))
	if !ok {
		t.Fatal("parseLaunchArgs() returned false")
	}
	if !opts.ephemeralCache {
		t.Error("ephemeralCache = false, want true")
	}
}

func TestParseLaunchArgsRejectsEphemeralWithoutSandbox(t *testing.T) {
	if _, ok := parseLaunchArgs("start", []string{"--ephemeral-cache", "--no-sandbox"}, devnullEnv(t)); ok {
		t.Error("parseLaunchArgs() succeeded with --ephemeral-cache and --no-sandbox")
	}
}

func TestBuildContinueOptsPreservesEphemeralCache(t *testing.T) {
	opts, code := buildContinueOpts([]string{"--ephemeral-cache"}, devnullEnv(t))
	if code != ExitOK {
		t.Fatalf("code = %d, want ExitOK", code)
	}
	if !opts.ephemeralCache {
		t.Error("ephemeralCache = false, want true")
	}
}

func TestBuildResumeOptsPreservesEphemeralCache(t *testing.T) {
	opts, code := buildResumeOpts([]string{"--ephemeral-cache"}, devnullEnv(t))
	if code != ExitOK {
		t.Fatalf("code = %d, want ExitOK", code)
	}
	if !opts.ephemeralCache {
		t.Error("ephemeralCache = false, want true")
	}
}

func TestLaunchCachePersistentAndEphemeral(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	sandboxTmp := t.TempDir()

	scope, xdgScope, err := prepareLaunchCache(true, false, "claude-code", workdir, sandboxTmp)
	if err != nil {
		t.Fatalf("no-sandbox cache: %v", err)
	}
	if scope != nil || xdgScope != nil {
		t.Errorf("no-sandbox scopes = %#v/%#v, want nil", scope, xdgScope)
	}

	ephemeral, ephemeralXDG, err := prepareLaunchCache(false, true, "claude-code", workdir, sandboxTmp)
	if err != nil {
		t.Fatalf("ephemeral cache: %v", err)
	}
	if ephemeral.Mode != toolcache.ModeEphemeral {
		t.Errorf("ephemeral mode = %q, want %q", ephemeral.Mode, toolcache.ModeEphemeral)
	}
	if ephemeral.Dir != filepath.Join(sandboxTmp, "cache") {
		t.Errorf("ephemeral dir = %q, want %q", ephemeral.Dir, filepath.Join(sandboxTmp, "cache"))
	}
	if ephemeralXDG != ephemeral {
		t.Errorf("ephemeral XDG scope should equal the build scope (single scope), got %#v", ephemeralXDG)
	}
	if _, err := os.Stat(filepath.Join(home, ".cache", "omac")); !os.IsNotExist(err) {
		t.Errorf("persistent cache root exists or stat failed: %v", err)
	}

	persistent, persistentXDG, err := prepareLaunchCache(false, false, "claude-code", workdir, sandboxTmp)
	if err != nil {
		t.Fatalf("persistent cache: %v", err)
	}
	t.Cleanup(func() {
		if err := persistent.Close(); err != nil {
			t.Errorf("close persistent cache: %v", err)
		}
		if err := persistentXDG.Close(); err != nil {
			t.Errorf("close persistent XDG cache: %v", err)
		}
	})
	if persistent.Domain != toolcache.DomainWorkdir {
		t.Errorf("persistent domain = %q, want %q", persistent.Domain, toolcache.DomainWorkdir)
	}
	if persistent.Mode != toolcache.ModePersistent {
		t.Errorf("persistent mode = %q, want %q", persistent.Mode, toolcache.ModePersistent)
	}
	if persistentXDG.Domain != toolcache.DomainHarness {
		t.Errorf("persistent XDG domain = %q, want %q", persistentXDG.Domain, toolcache.DomainHarness)
	}
	if persistentXDG.Dir == persistent.Dir {
		t.Errorf("harness XDG scope should differ from the workdir build scope: %s", persistentXDG.Dir)
	}
}

func TestLaunchCacheInjectsSelectedScope(t *testing.T) {
	for _, test := range []struct {
		name      string
		ephemeral bool
		mode      toolcache.Mode
	}{
		{name: "persistent", mode: toolcache.ModePersistent},
		{name: "ephemeral", ephemeral: true, mode: toolcache.ModeEphemeral},
	} {
		t.Run(test.name, func(t *testing.T) {
			capture := launchCacheCapture(t, false, test.ephemeral, true)
			cacheEnv := toolcache.Environment(capture.env["OMAC_CACHE_DIR"], test.mode)
			for _, key := range []string{"OMAC_CACHE_DIR", "OMAC_CACHE_MODE"} {
				if got := capture.env[key]; got != cacheEnv[key] {
					t.Errorf("%s = %q, want %q", key, got, cacheEnv[key])
				}
			}
			assertCacheScopeAllowed(t, capture.args, cacheEnv["OMAC_CACHE_DIR"])

			line := fmt.Sprintf("[verbose] cache mode=%s path=%s", test.mode, cacheEnv["OMAC_CACHE_DIR"])
			if strings.Count(capture.stderr, line) != 1 {
				t.Errorf("verbose cache line count = %d, want 1\nstderr:\n%s", strings.Count(capture.stderr, line), capture.stderr)
			}

			if test.ephemeral {
				if _, err := os.Stat(filepath.Join(capture.home, ".cache", "omac")); !os.IsNotExist(err) {
					t.Errorf("persistent cache root exists or stat failed: %v", err)
				}
				return
			}
			cleared, err := toolcache.ClearWorkdir(capture.workdir)
			if err != nil {
				t.Fatalf("clear workdir cache: %v", err)
			}
			if cleared.Status != toolcache.ClearRemoved {
				t.Errorf("clear status = %q, want %q (launch should close the scope)", cleared.Status, toolcache.ClearRemoved)
			}
		})
	}
}

func TestLaunchCachePersistentSetupFailureHasRecoveryHint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	workdir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".cache", "omac"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	env, stderr := launchTestEnv(t, workdir)
	harness, ok := config.LookupHarness("claude")
	if !ok {
		t.Fatal("claude harness missing")
	}
	code := runLaunch(env, launchOpts{label: "start", harness: harness, innerCmdOverride: "/bin/true"})
	if code != ExitIOError {
		t.Fatalf("code = %d, want ExitIOError", code)
	}
	if output := stderr(); !strings.Contains(output, "retry with --ephemeral-cache to bypass persistent cache setup") {
		t.Errorf("stderr missing recovery hint:\n%s", output)
	}
}

func TestLaunchCacheOmitsVerboseOutputWithoutVerbose(t *testing.T) {
	capture := launchCacheCapture(t, false, false, false)
	if strings.Contains(capture.stderr, "[verbose] cache mode=") {
		t.Errorf("non-verbose launch reported cache mode:\n%s", capture.stderr)
	}
}

func TestLaunchCacheNoSandboxPreservesHostEnvironment(t *testing.T) {
	capture := launchCacheCapture(t, true, false, false)
	for key := range toolcache.Environment("ignored", toolcache.ModePersistent) {
		want := "host-" + key
		if got := capture.env[key]; got != want {
			t.Errorf("%s = %q, want host value %q", key, got, want)
		}
	}
	for i, arg := range capture.args {
		if arg == "--allow" {
			t.Errorf("argv contains --allow at index %d: %v", i, capture.args)
		}
	}
	if _, err := os.Stat(filepath.Join(capture.home, ".cache", "omac")); !os.IsNotExist(err) {
		t.Errorf("cache root exists or stat failed: %v", err)
	}
	if strings.Contains(capture.stderr, "[verbose] cache mode=") {
		t.Errorf("non-verbose launch reported cache mode:\n%s", capture.stderr)
	}
}

type cacheCapture struct {
	args    []string
	env     map[string]string
	home    string
	workdir string
	stderr  string
}

func launchCacheCapture(t *testing.T, noSandbox, ephemeral, verbose bool) cacheCapture {
	t.Helper()
	shortTmp, err := os.MkdirTemp("/tmp", "omac-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shortTmp) })
	t.Setenv("TMPDIR", shortTmp)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	for key := range toolcache.Environment("ignored", toolcache.ModePersistent) {
		t.Setenv(key, "host-"+key)
	}

	workdir := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "args")
	envPath := filepath.Join(t.TempDir(), "env")
	t.Setenv("OMAC_TEST_ARGS", argsPath)
	t.Setenv("OMAC_TEST_ENV", envPath)
	capturePath := filepath.Join(t.TempDir(), "capture")
	if err := os.WriteFile(capturePath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OMAC_TEST_ARGS\"\nenv > \"$OMAC_TEST_ENV\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !noSandbox {
		configPath := filepath.Join(workdir, ".opencode", "oh-my-agentic-coder.yaml")
		if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
			t.Fatal(err)
		}
		configText := fmt.Sprintf("sandbox:\n  default_profile: capture\n  profiles:\n    capture:\n      command: [%q, %q, %q, %q]\n", capturePath, "--", "{{inner_cmd}}", "{{inner_args}}")
		if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	env, stderr := launchTestEnv(t, workdir)
	harness, ok := config.LookupHarness("claude")
	if !ok {
		t.Fatal("claude harness missing")
	}
	inner := "/bin/true"
	if noSandbox {
		inner = capturePath
	}
	code := runLaunch(env, launchOpts{
		label:            "start",
		harness:          harness,
		innerCmdOverride: inner,
		noSandbox:        noSandbox,
		ephemeralCache:   ephemeral,
		verbose:          verbose,
	})
	if code != ExitOK {
		t.Fatalf("runLaunch() = %d, want ExitOK\nstderr:\n%s", code, stderr())
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	envData, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read captured environment: %v", err)
	}
	return cacheCapture{
		args:    strings.Fields(string(args)),
		env:     parseEnvironment(string(envData)),
		home:    home,
		workdir: workdir,
		stderr:  stderr(),
	}
}

func launchTestEnv(t *testing.T, workdir string) (*Env, func() string) {
	t.Helper()
	stdout, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdout.Close()
		stderr.Close()
		stdin.Close()
	})
	return &Env{Version: "test", Workdir: workdir, Stdout: stdout, Stderr: stderr, Stdin: stdin}, func() string {
		t.Helper()
		if err := stderr.Sync(); err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(stderr.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(contents)
	}
}

func parseEnvironment(data string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			env[key] = value
		}
	}
	return env
}

func assertCacheScopeAllowed(t *testing.T, args []string, want string) {
	t.Helper()
	count := 0
	for i, arg := range args {
		if arg == "--allow" && i+1 < len(args) {
			if args[i+1] == want {
				count++
			}
			if args[i+1] == filepath.Dir(want) {
				t.Errorf("argv grants cache root %q instead of only the selected scope: %v", filepath.Dir(want), args)
			}
		}
	}
	if count != 1 {
		t.Errorf("selected scope %q appears in --allow %d times, want 1: %v", want, count, args)
	}
}

func TestBuildResumeInnerArgs(t *testing.T) {
	oc, _ := config.LookupHarness("opencode")
	cc, _ := config.LookupHarness("claude-code")

	if got := buildResumeInnerArgs(oc.Session, "ses_X", nil); !reflect.DeepEqual(got, []string{"--session", "ses_X"}) {
		t.Errorf("opencode resume args = %v, want [--session ses_X]", got)
	}
	if got := buildResumeInnerArgs(cc.Session, "uuid-1", nil); !reflect.DeepEqual(got, []string{"--resume", "uuid-1"}) {
		t.Errorf("claude resume args = %v, want [--resume uuid-1]", got)
	}
	// User-supplied inner args follow the resume flag.
	if got := buildResumeInnerArgs(oc.Session, "ses_X", []string{"--model", "y"}); !reflect.DeepEqual(got, []string{"--session", "ses_X", "--model", "y"}) {
		t.Errorf("resume args with user inner = %v", got)
	}
}

func TestParseSelection(t *testing.T) {
	cases := []struct {
		line    string
		n       int
		wantIdx int
		wantOK  bool
	}{
		{"1", 3, 0, true},
		{"3", 3, 2, true},
		{"  2 ", 3, 1, true},
		{"", 3, 0, false},   // cancel
		{"0", 3, 0, false},  // out of range low
		{"4", 3, 0, false},  // out of range high
		{"x", 3, 0, false},  // non-numeric
		{"-1", 3, 0, false}, // negative
	}
	for _, c := range cases {
		idx, ok := parseSelection(c.line, c.n)
		if idx != c.wantIdx || ok != c.wantOK {
			t.Errorf("parseSelection(%q, %d) = (%d,%v), want (%d,%v)", c.line, c.n, idx, ok, c.wantIdx, c.wantOK)
		}
	}
}

func TestPickSessionNonTTY(t *testing.T) {
	// Stdin from a pipe is not a TTY, so pickSession must print the list and
	// return false without blocking on input.
	r, _, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	null, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	defer null.Close()
	env := &Env{Version: "test", Workdir: "/w", Stdout: null, Stderr: null, Stdin: r}

	sessions := []session.Session{{ID: "a", Title: "first"}, {ID: "b", Title: "second"}}
	if _, ok := pickSession(env, "opencode", sessions); ok {
		t.Error("non-TTY stdin should not yield a selection")
	}
}

func TestRelativeTime(t *testing.T) {
	if got := relativeTime(time.Time{}); got != "unknown" {
		t.Errorf("zero time = %q, want unknown", got)
	}
	if got := relativeTime(time.Now().Add(-2 * time.Hour)); got != "2h ago" {
		t.Errorf("2h ago = %q", got)
	}
	if got := relativeTime(time.Now().Add(-49 * time.Hour)); got != "2d ago" {
		t.Errorf("2d ago = %q", got)
	}
}

func TestContinueHintToken(t *testing.T) {
	oc, _ := config.LookupHarness("opencode")
	cc, _ := config.LookupHarness("claude-code")
	if got := continueHintToken(oc); got != "" {
		t.Errorf("opencode token = %q, want empty (default harness)", got)
	}
	if got := continueHintToken(cc); got != " claude" {
		t.Errorf("claude token = %q, want \" claude\" (first alias)", got)
	}
}
