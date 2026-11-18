package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestServePassword(t *testing.T) {
	h, ok := config.LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}
	for _, tc := range []struct {
		name                       string
		password                   string
		deny                       []string
		native, noSandbox, wantErr bool
	}{
		{name: "unset", native: true},
		{name: "dedicated", password: "serve-only", native: true},
		{name: "denied", password: "serve-only", native: true, deny: []string{"OPENCODE_*"}, wantErr: true},
		{name: "opaque backend", password: "serve-only", wantErr: true},
		{name: "unsandboxed", password: "serve-only", noSandbox: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENCODE_SERVER_PASSWORD", "ambient-secret")
			plan := sandboxPlan{Native: tc.native, Policy: &sandboxprofile.Profile{Environment: sandboxprofile.Environment{DenyVars: tc.deny}}}
			extra, err := prepareServeAuthEnv(tc.password, h, plan, tc.noSandbox)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if err != nil {
				if strings.Contains(err.Error(), "serve-only") {
					t.Fatal("password in error")
				}
				return
			}
			if extra["OPENCODE_SERVER_PASSWORD"] != tc.password {
				t.Fatal("dedicated password missing or ambient password forwarded")
			}
			// The hint must see what the server sees: in the sandbox the
			// ambient value is filtered out, and with --no-sandbox the
			// dedicated password takes precedence over it.
			if got := serveAuthGetenv(h, plan, extra, tc.noSandbox)("OPENCODE_SERVER_PASSWORD"); got != tc.password {
				t.Fatalf("hint sees password %q, want %q", got, tc.password)
			}
		})
	}
}

func TestServeAuthRespectsExplicitAllowance(t *testing.T) {
	t.Setenv("OPENCODE_SERVER_PASSWORD", "explicitly-allowed")
	h, _ := config.LookupHarness("opencode")
	plan := sandboxPlan{Native: true, Policy: &sandboxprofile.Profile{Environment: sandboxprofile.Environment{AllowVars: []string{"OPENCODE_SERVER_PASSWORD"}}}}
	if serveAuthGetenv(h, plan, nil, false)("OPENCODE_SERVER_PASSWORD") != "explicitly-allowed" {
		t.Fatal("explicit allowance ignored")
	}
	// Passing the dedicated password through the real filter must not require
	// forwarding unrelated host credentials.
	extra, err := prepareServeAuthEnv("dedicated", h, plan, false)
	if err != nil {
		t.Fatal(err)
	}
	child := sandboxprofile.FilterEnv([]string{"PROVIDER_KEY=secret"}, []string{"OPENCODE_SERVER_PASSWORD"}, nil, extra)
	if !slices.Contains(child, "OPENCODE_SERVER_PASSWORD=dedicated") || slices.Contains(child, "PROVIDER_KEY=secret") {
		t.Fatal("incorrect child environment")
	}
}

func TestServePasswordErrorsNameTheSetting(t *testing.T) {
	h, _ := config.LookupHarness("opencode")
	for _, tc := range []struct {
		name string
		plan sandboxPlan
		want string
	}{
		{
			name: "other backend",
			plan: sandboxPlan{Name: "nono"},
			want: `OMAC_SERVE_PASSWORD only works with the native sandbox backend, and sandbox runtime "nono" uses another one. ` +
				"Unset OMAC_SERVE_PASSWORD, or set OPENCODE_SERVER_PASSWORD yourself where that backend runs the server",
		},
		{
			name: "policy not loaded",
			plan: sandboxPlan{Name: "builtin", Native: true, PolicyRef: "broken", PolicyErr: errors.New("bad JSON")},
			want: `OMAC_SERVE_PASSWORD needs sandbox profile "broken" to check its environment.deny_vars, but it could not be loaded: bad JSON. ` +
				"Fix that profile or unset OMAC_SERVE_PASSWORD",
		},
		{
			name: "denied",
			plan: sandboxPlan{Name: "builtin", Native: true, PolicyRef: "strict", Policy: &sandboxprofile.Profile{Environment: sandboxprofile.Environment{DenyVars: []string{"OPENCODE_*"}}}},
			want: `OMAC_SERVE_PASSWORD is passed to the server as OPENCODE_SERVER_PASSWORD, but sandbox profile "strict" blocks that variable in environment.deny_vars. ` +
				"Remove the matching entry or unset OMAC_SERVE_PASSWORD",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := prepareServeAuthEnv("serve-only", h, tc.plan, false)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v\nwant    %s", err, tc.want)
			}
		})
	}
}

// The password must reach the environment runServe hands to the inner
// server, together with the marker the supervisor checks.
func TestServePasswordReachesInnerEnv(t *testing.T) {
	h, ok := config.LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}
	plan := sandboxPlan{Name: "builtin", Native: true, PolicyRef: "default", Policy: &sandboxprofile.Profile{}}
	authExtra, err := prepareServeAuthEnv("serve-only", h, plan, false)
	if err != nil {
		t.Fatal(err)
	}
	extra := map[string]string{"OMAC_BASE": "http://127.0.0.1:1/"}
	addServeAuthEnv(extra, authExtra)
	if extra["OPENCODE_SERVER_PASSWORD"] != "serve-only" {
		t.Errorf("OPENCODE_SERVER_PASSWORD = %q, want the dedicated password", extra["OPENCODE_SERVER_PASSWORD"])
	}
	if extra[sandboxprofile.ServeAuthTargetEnv] != "OPENCODE_SERVER_PASSWORD" {
		t.Errorf("%s = %q, want OPENCODE_SERVER_PASSWORD", sandboxprofile.ServeAuthTargetEnv, extra[sandboxprofile.ServeAuthTargetEnv])
	}
	if extra["OMAC_BASE"] == "" {
		t.Error("base environment lost")
	}
}

// The audit log must never contain the server password, even if it ends up
// on a logged argv.
func TestServeAuditRedactsPassword(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	cfg, misuse := serveAuditConfig(config.LauncherConfig{}, auditFlags{logPath: logPath}, "test", "serve-only-secret", nil)
	if misuse != "" {
		t.Fatal(misuse)
	}
	auditor, err := audit.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	auditor.Emit(audit.InnerExec([]string{"opencode", "serve", "--password", "serve-only-secret"}, "builtin", true))
	if err := auditor.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"opencode"`) {
		t.Fatalf("argv not logged at all: %s", data)
	}
	if strings.Contains(string(data), "serve-only-secret") {
		t.Errorf("password written to the audit log: %s", data)
	}
}

// The hint about the random password must disappear once omac supplies a
// password, whether the server runs in the sandbox or not.
func TestServePasswordSuppressesHint(t *testing.T) {
	h, ok := config.LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}
	t.Setenv("OPENCODE_SERVER_PASSWORD", "")
	os.Unsetenv("OPENCODE_SERVER_PASSWORD")
	plan := sandboxPlan{Name: "builtin", Native: true, PolicyRef: "default", Policy: &sandboxprofile.Profile{Environment: sandboxprofile.Environment{AllowVars: []string{"PATH"}}}}
	inner := []string{"opencode", "serve"}
	for _, noSandbox := range []bool{false, true} {
		for _, password := range []string{"", "serve-only"} {
			extra, err := prepareServeAuthEnv(password, h, plan, noSandbox)
			if err != nil {
				t.Fatal(err)
			}
			for _, generates := range []bool{false, true} {
				hint := serverExposureWarning(h, inner, serveAuthGetenv(h, plan, extra, noSandbox), serveAuthAdvice(h, plan, noSandbox), generates)
				if (hint == "") != (password != "") {
					t.Errorf("noSandbox=%v password=%q generatesPassword=%v: hint = %q", noSandbox, password, generates, hint)
				}
			}
		}
	}
}

// The hint must never recommend OMAC_SERVE_PASSWORD where the same run would
// reject it, and must read the host environment for another sandbox backend.
func TestServePasswordHintAdvice(t *testing.T) {
	h, ok := config.LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}
	const prefix = `omac serve: opencode server on 127.0.0.1:4096 uses a random password that changes on every restart ` +
		`(opencode prints it as "server password …"). To keep the same password, `
	inner := []string{"opencode", "serve"}
	for _, tc := range []struct {
		name string
		plan sandboxPlan
		want string
	}{
		{
			name: "native",
			plan: sandboxPlan{Name: "builtin", Native: true, PolicyRef: "default", Policy: &sandboxprofile.Profile{}},
			want: prefix + "set OMAC_SERVE_PASSWORD.",
		},
		{
			name: "other backend",
			plan: sandboxPlan{Name: "nono"},
			want: prefix + "set OPENCODE_SERVER_PASSWORD where your sandbox backend runs the server.",
		},
		{
			name: "policy not loaded",
			plan: sandboxPlan{Name: "builtin", Native: true, PolicyRef: "broken", PolicyErr: os.ErrNotExist},
			want: prefix + `fix sandbox profile "broken", which could not be loaded, then set OMAC_SERVE_PASSWORD.`,
		},
		{
			name: "denied",
			plan: sandboxPlan{Name: "builtin", Native: true, PolicyRef: "strict", Policy: &sandboxprofile.Profile{Environment: sandboxprofile.Environment{DenyVars: []string{"OPENCODE_*"}}}},
			want: prefix + `remove the entry matching OPENCODE_SERVER_PASSWORD from environment.deny_vars in sandbox profile "strict", then set OMAC_SERVE_PASSWORD.`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENCODE_SERVER_PASSWORD", "")
			os.Unsetenv("OPENCODE_SERVER_PASSWORD")
			advice := serveAuthAdvice(h, tc.plan, false)
			getenv := serveAuthGetenv(h, tc.plan, nil, false)
			if got := serverExposureWarning(h, inner, getenv, advice, true); got != tc.want {
				t.Errorf("hint = %q\nwant   %q", got, tc.want)
			}
			// The OpenCode 1.x warning carries the same advice.
			if got := serverExposureWarning(h, inner, getenv, advice, false); !strings.HasSuffix(got, "to require a password, "+advice+".") {
				t.Errorf("OpenCode 1.x hint = %q, want it to end with the advice %q", got, advice)
			}
			// Advice that simply says to set OMAC_SERVE_PASSWORD must hold.
			_, err := prepareServeAuthEnv("serve-only", h, tc.plan, false)
			if advice == "set OMAC_SERVE_PASSWORD" && err != nil {
				t.Errorf("hint recommends OMAC_SERVE_PASSWORD, but it is rejected: %v", err)
			}
		})
	}

	// Another backend starts from the host environment, so a password set
	// there must silence the hint, as it did before OMAC_SERVE_PASSWORD.
	t.Setenv("OPENCODE_SERVER_PASSWORD", "from-shell")
	plan := sandboxPlan{Name: "nono"}
	if got := serverExposureWarning(h, inner, serveAuthGetenv(h, plan, nil, false), serveAuthAdvice(h, plan, false), false); got != "" {
		t.Errorf("other backend with OPENCODE_SERVER_PASSWORD set: hint = %q", got)
	}
}

// The sandbox lets the auth variable through only when omac supplies a
// password; otherwise a shell value of it must stay filtered out.
func TestServePasswordAllowsAuthVarOnlyWithPassword(t *testing.T) {
	h, ok := config.LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}
	plan := sandboxPlan{Name: "builtin", Native: true, PolicyRef: "default", Policy: &sandboxprofile.Profile{}}
	base := []string{"omac", "sandbox", "run", "--profile", "default", "--", "opencode", "serve"}
	allowsAuth := func(argv []string) bool {
		for i := 0; i+1 < len(argv) && argv[i] != "--"; i++ {
			if argv[i] == "--allow-env" && argv[i+1] == "OPENCODE_SERVER_PASSWORD" {
				return true
			}
		}
		return false
	}
	if !allowsAuth(injectServeAuthAllow(slices.Clone(base), h, plan, "serve-only")) {
		t.Error("with a password: --allow-env OPENCODE_SERVER_PASSWORD missing")
	}
	if allowsAuth(injectServeAuthAllow(slices.Clone(base), h, plan, "")) {
		t.Error("without a password: OPENCODE_SERVER_PASSWORD must not be allowed")
	}
}

// runServe must remove OMAC_SERVE_PASSWORD from its own environment first,
// so host-side processes it starts (skill sidecars) never inherit it.
func TestRunServeConsumesPassword(t *testing.T) {
	t.Setenv(sandboxprofile.ServePasswordEnv, "serve-only")
	env, _ := launchTestEnv(t, t.TempDir())
	if code := runServe([]string{"--no-such-flag"}, env); code != ExitMisuse {
		t.Fatalf("exit = %d, want ExitMisuse (%d)", code, ExitMisuse)
	}
	if _, set := os.LookupEnv(sandboxprofile.ServePasswordEnv); set {
		t.Errorf("%s is still set in the omac process environment", sandboxprofile.ServePasswordEnv)
	}
}

func TestRunServeRejectsPasswordWithNoInner(t *testing.T) {
	t.Setenv(sandboxprofile.ServePasswordEnv, "serve-only")
	env, stderr := launchTestEnv(t, t.TempDir())
	if code := runServe([]string{"opencode", "--no-inner", "--no-sandbox"}, env); code != ExitMisuse {
		t.Fatalf("exit = %d, want ExitMisuse (%d)", code, ExitMisuse)
	}
	want := "omac serve: OMAC_SERVE_PASSWORD has no effect with --no-inner (omac starts no server); unset it or drop --no-inner."
	if got := stderr(); !strings.Contains(got, want) {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// An empty OMAC_SERVE_PASSWORD (e.g. from a failed keychain lookup) must not
// silently fall back to a random password.
func TestRunServeRejectsEmptyPassword(t *testing.T) {
	t.Setenv(sandboxprofile.ServePasswordEnv, "")
	env, stderr := launchTestEnv(t, t.TempDir())
	if code := runServe([]string{"opencode", "--no-sandbox"}, env); code != ExitMisuse {
		t.Fatalf("exit = %d, want ExitMisuse (%d)", code, ExitMisuse)
	}
	want := "omac serve: OMAC_SERVE_PASSWORD is set but empty; set a password or unset it."
	if got := stderr(); !strings.Contains(got, want) {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}
