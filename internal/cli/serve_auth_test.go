package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestServeConsumesPasswordBeforeStartup(t *testing.T) {
	t.Setenv("OMAC_SERVE_PASSWORD", "dedicated-secret")
	env, _, stderr, drain := newPipeEnv(t, "")
	if code := runServe([]string{"--invalid-option"}, env); code != ExitMisuse {
		t.Fatalf("exit = %d, want misuse", code)
	}
	drain()
	if _, present := os.LookupEnv("OMAC_SERVE_PASSWORD"); present {
		t.Error("host source must be consumed even when startup fails")
	}
	if strings.Contains(stderr.String(), "dedicated-secret") {
		t.Error("startup diagnostics leaked the dedicated password")
	}
}

func TestServeAuthDeniedPasswordStopsBeforeLaunch(t *testing.T) {
	isolateHome(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("OMAC_SERVE_PASSWORD", "dedicated-secret")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "ambient-secret")
	stageProfile(t, os.Getenv("HOME"), `{"meta":{"name":"default"},"environment":{"allow_vars":["*"],"deny_vars":["OPENCODE_*"]}}`)
	env, stdout, stderr, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	code := runServe([]string{"--inner", "/must-not-launch", "--verbose"}, env)
	drain()
	if code != ExitConfigInvalid || !strings.Contains(stderr.String(), "deny_vars") {
		t.Fatalf("dedicated deny must stop launch with a policy diagnostic; exit=%d, stderr=%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Error("serve started before refusing the explicit password")
	}
	for _, secret := range []string{"dedicated-secret", "ambient-secret"} {
		if strings.Contains(stderr.String(), secret) {
			t.Error("launch failure leaked an auth value")
		}
	}
	if _, present := os.LookupEnv("OMAC_SERVE_PASSWORD"); present {
		t.Error("host source survived startup refusal")
	}
	if os.Getenv("OPENCODE_SERVER_PASSWORD") != "ambient-secret" {
		t.Error("dedicated target must not be installed in the sidecars' host environment")
	}
}

func TestPrepareServeAuthEnv(t *testing.T) {
	h, _ := config.LookupHarness("opencode")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "ambient-secret")
	for _, tc := range []struct {
		name        string
		password    string
		allow, deny []string
		want        string
		wantErr     bool
	}{
		{name: "dedicated overrides ambient", password: "dedicated-secret", allow: []string{"HOME"}, want: "dedicated-secret"},
		{name: "ambient has no automatic exception", allow: []string{"HOME"}},
		{name: "explicit ambient allowance preserved", allow: []string{"OPENCODE_SERVER_PASSWORD"}, want: "ambient-secret"},
		{name: "dedicated exact deny", password: "dedicated-secret", deny: []string{"OPENCODE_SERVER_PASSWORD"}, wantErr: true},
		{name: "dedicated prefix deny", password: "dedicated-secret", deny: []string{"OPENCODE_*"}, wantErr: true},
		{name: "dedicated wildcard deny", password: "dedicated-secret", deny: []string{"*"}, wantErr: true},
		{name: "ambient denied despite allowance", allow: []string{"*"}, deny: []string{"OPENCODE_*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := sandboxPlan{Native: true, Policy: &sandboxprofile.Profile{Environment: sandboxprofile.Environment{AllowVars: tc.allow, DenyVars: tc.deny}}}
			extra, err := prepareServeAuthEnv(tc.password, h, plan, false)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if err != nil {
				if !strings.Contains(err.Error(), "OMAC_SERVE_PASSWORD") || !strings.Contains(err.Error(), "deny_vars") || strings.Contains(err.Error(), "dedicated-secret") || strings.Contains(err.Error(), "ambient-secret") {
					t.Fatalf("error must be actionable and secret-free: %v", err)
				}
				return
			}
			getenv := serveAuthGetenv(h, plan, extra, false)
			if got := getenv("OPENCODE_SERVER_PASSWORD"); got != tc.want {
				t.Error("effective password does not match the intended allowed value")
			}
			if _, leaked := extra["OMAC_SERVE_PASSWORD"]; leaked {
				t.Error("source copied to child overlay")
			}
			if tc.password == "" && len(extra) != 0 {
				t.Error("normal harness behavior must not inject auth vars")
			}
			if tc.password != "" && (extra["OPENCODE_SERVER_PASSWORD"] != "dedicated-secret" || extra["OMAC_SERVE_AUTH_TARGET"] != "OPENCODE_SERVER_PASSWORD" || len(extra) != 2) {
				t.Error("only target and non-secret final-filter requirement should be injected")
			}
			warning := serverExposureWarning(h, []string{"opencode", "serve"}, getenv)
			if (warning == "") != (tc.want != "") {
				t.Errorf("warning does not reflect effective filtering: %q", warning)
			}
		})
	}
}

func TestPrepareServeAuthEnvUnsupportedLaunch(t *testing.T) {
	h, _ := config.LookupHarness("opencode")
	for _, tc := range []struct {
		name      string
		harness   config.Harness
		plan      sandboxPlan
		noSandbox bool
		wantErr   string
	}{
		{name: "opaque backend", harness: h, wantErr: "cannot verify"},
		{name: "unresolved policy", harness: h, plan: sandboxPlan{Native: true}, wantErr: "policy"},
		{name: "no auth target", harness: config.Harness{Name: "custom"}, plan: sandboxPlan{Native: true}, wantErr: "AuthEnvVar"},
		{name: "server without auth variable", harness: config.Harness{Name: "custom", ServerLaunch: &config.ServerLaunch{}}, plan: sandboxPlan{Native: true}, wantErr: "AuthEnvVar"},
		{name: "direct launch", harness: h, noSandbox: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra, err := prepareServeAuthEnv("dedicated-secret", tc.harness, tc.plan, tc.noSandbox)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || strings.Contains(err.Error(), "dedicated-secret") {
					t.Fatalf("expected secret-free %q failure, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(extra) != 1 || extra["OPENCODE_SERVER_PASSWORD"] != "dedicated-secret" {
				t.Error("direct launch must receive only the target")
			}
		})
	}
	// With no dedicated password neither an opaque backend nor an unsupported
	// harness changes the harness's normal behavior or claims verified forwarding.
	for _, harness := range []config.Harness{h, {Name: "custom"}} {
		extra, err := prepareServeAuthEnv("", harness, sandboxPlan{}, false)
		if err != nil || len(extra) != 0 {
			t.Fatalf("no password must preserve normal behavior: %v", err)
		}
	}
	getenv := serveAuthGetenv(h, sandboxPlan{}, nil, false)
	if getenv("OPENCODE_SERVER_PASSWORD") != "" {
		t.Error("opaque backend cannot claim verified ambient forwarding")
	}
}

const serveAuthLaunchPassword = "serve-launch-dedicated-fixture"
const serveAuthLaunchAmbient = "serve-launch-ambient-fixture"

// TestMain dispatches this helper only in the exec'd child. It checks the
// real launch environment/argv without writing or printing password values.
// Native mode stops at the supervisor boundary, without nesting an OS sandbox.
func serveAuthLaunchChild(mode string) int {
	fail := func(message string) int {
		fmt.Fprintln(os.Stderr, "serve auth launch probe:", message)
		return 81
	}
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Fprintln(os.Stdout, "2.1.0")
		return 0
	}
	if os.Getenv("OPENCODE_SERVER_PASSWORD") != serveAuthLaunchPassword {
		return fail("dedicated password did not override ambient target")
	}
	if _, present := os.LookupEnv("OMAC_SERVE_PASSWORD"); present {
		return fail("host-only password source reached child")
	}
	for _, arg := range os.Args {
		if strings.Contains(arg, serveAuthLaunchPassword) || strings.Contains(arg, serveAuthLaunchAmbient) {
			return fail("password value appeared in actual launch argv")
		}
		if strings.Contains(arg, "OMAC_SERVE_PASSWORD") || strings.Contains(arg, "OMAC_SERVE_AUTH_TARGET") {
			return fail("host-only auth metadata appeared in actual launch argv")
		}
	}
	switch mode {
	case "direct":
		if len(os.Args) < 2 || os.Args[1] != "serve" {
			return fail("direct server child was not launched")
		}
		if _, present := os.LookupEnv("OMAC_SERVE_AUTH_TARGET"); present {
			return fail("direct server received native supervisor marker")
		}
	case "native":
		if len(os.Args) < 4 || os.Args[1] != "sandbox" || os.Args[2] != "run" {
			return fail("native supervisor boundary was not reached")
		}
		if os.Getenv("OMAC_SERVE_AUTH_TARGET") != "OPENCODE_SERVER_PASSWORD" {
			return fail("native auth-target marker missing")
		}
		flags, err := sandboxprofile.ParseFlags(os.Args[3:])
		if err != nil {
			return fail("invalid native launch flags")
		}
		if !sandboxprofile.EnvVarMatches("OPENCODE_SERVER_PASSWORD", flags.AllowEnv) {
			return fail("native target allowance missing from actual launch argv")
		}
		profile, _, err := sandboxprofile.Resolve(flags.ProfileRef)
		if err != nil {
			return fail("cannot resolve native launch policy")
		}
		merged, _ := sandboxprofile.Merge(profile, flags)
		filtered := parseEnvironment(strings.Join(sandboxprofile.FilterEnv(os.Environ(), sandboxprofile.EffectiveAllowVars(merged.Environment.AllowVars), merged.Environment.DenyVars, nil), "\n"))
		if filtered["OPENCODE_SERVER_PASSWORD"] != serveAuthLaunchPassword || filtered["OMAC_SERVE_PASSWORD"] != "" || filtered["OMAC_SERVE_AUTH_TARGET"] != "" {
			return fail("actual native allowance did not produce the intended filtered environment")
		}
	default:
		return fail("unknown helper mode")
	}
	if err := os.WriteFile(os.Getenv("OMAC_TEST_SERVE_AUTH_CHECKED"), []byte(mode), 0600); err != nil {
		return fail("cannot record non-secret child completion")
	}
	return 0
}

func TestDedicatedServeAuthSuccessfulLaunch(t *testing.T) {
	for _, mode := range []string{"direct", "native"} {
		t.Run(mode, func(t *testing.T) {
			isolateHome(t)
			root := t.TempDir()
			t.Chdir(root)
			// Relative temp paths keep the real Unix socket under the kernel's
			// path-length limit without requiring access to shared /tmp.
			t.Setenv("TMPDIR", ".")
			t.Setenv("XDG_RUNTIME_DIR", root)
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
			t.Setenv("OMAC_SERVE_PASSWORD", serveAuthLaunchPassword)
			t.Setenv("OPENCODE_SERVER_PASSWORD", serveAuthLaunchAmbient)
			t.Setenv("OMAC_SERVE_AUTH_TARGET", "")
			// Direct children must not receive even an empty native marker.
			if err := os.Unsetenv("OMAC_SERVE_AUTH_TARGET"); err != nil {
				t.Fatal(err)
			}
			t.Setenv("OMAC_TEST_SERVE_AUTH_LAUNCH", mode)
			checked := filepath.Join(root, "child-checked")
			t.Setenv("OMAC_TEST_SERVE_AUTH_CHECKED", checked)
			stageProfile(t, os.Getenv("HOME"), `{"meta":{"name":"default"},"environment":{"allow_vars":["HOME"]}}`)
			child, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			auditPath := filepath.Join(root, "audit.jsonl")
			args := []string{"--inner", child, "--verbose", "--audit-log", auditPath, "--audit-strict"}
			if mode == "direct" {
				args = append(args, "--no-sandbox")
			} else {
				args = append(args, "--ephemeral-cache")
			}
			env, stderr := launchTestEnv(t, root)
			if code := runServe(args, env); code != ExitOK {
				diagnostics := strings.NewReplacer(serveAuthLaunchPassword, "<dedicated-redacted>", serveAuthLaunchAmbient, "<ambient-redacted>").Replace(stderr())
				t.Fatalf("runServe successful-launch probe exited %d; diagnostics: %s", code, diagnostics)
			}
			completion, err := os.ReadFile(checked)
			if err != nil || string(completion) != mode {
				t.Fatal("runServe did not reach and pass the child environment checks")
			}
			verbose := stderr()
			if !strings.Contains(verbose, "[verbose] inner argv:") {
				t.Error("actual verbose launch output missing")
			}
			log, err := os.ReadFile(auditPath)
			if err != nil {
				t.Fatal(err)
			}
			var sawExec, sawStop bool
			for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
				var event audit.Event
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatal(err)
				}
				if event.Type == audit.TypeProcessExec {
					sawExec = len(event.Argv) > 0 && event.Sandboxed != nil && *event.Sandboxed == (mode == "native")
				}
				if event.Type == audit.TypeSessionStop {
					sawStop = event.ExitCode != nil && *event.ExitCode == 0
				}
			}
			if !sawExec || !sawStop {
				t.Error("actual audit log lacks successful launch/stop events")
			}
			stdout, err := os.ReadFile(env.Stdout.Name())
			if err != nil {
				t.Fatal(err)
			}
			for _, output := range []string{verbose, string(log), string(stdout)} {
				if strings.Contains(output, serveAuthLaunchPassword) || strings.Contains(output, serveAuthLaunchAmbient) {
					t.Error("actual output or audit leaked a password value")
				}
				if strings.Contains(output, "OMAC_SERVE_PASSWORD") || strings.Contains(output, "OMAC_SERVE_AUTH_TARGET") {
					t.Error("actual output or audit exposed host-only auth metadata")
				}
			}
			if strings.Contains(verbose, "no fixed password") {
				t.Error("successful dedicated launch incorrectly warned of missing fixed password")
			}
			if _, present := os.LookupEnv("OMAC_SERVE_PASSWORD"); present {
				t.Error("source survived successful launch")
			}
			if os.Getenv("OPENCODE_SERVER_PASSWORD") != serveAuthLaunchAmbient {
				t.Error("child auth overlay modified sidecars' host environment")
			}
		})
	}
}

func TestServeAuthUsesSelectedHarnessTarget(t *testing.T) {
	h := config.Harness{Name: "custom", ServerLaunch: &config.ServerLaunch{AuthEnvVar: "CUSTOM_SERVER_AUTH"}}
	plan := sandboxPlan{Native: true, Policy: &sandboxprofile.Profile{}}
	extra, err := prepareServeAuthEnv("dedicated-secret", h, plan, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) != 2 || extra["CUSTOM_SERVER_AUTH"] != "dedicated-secret" || extra["OMAC_SERVE_AUTH_TARGET"] != "CUSTOM_SERVER_AUTH" {
		t.Error("dedicated password must map only to the selected descriptor's target")
	}
}
