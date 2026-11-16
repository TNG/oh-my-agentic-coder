package cli

import (
	"os"
	"slices"
	"strings"
	"testing"

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
			if got := serveAuthGetenv(h, plan, extra, tc.noSandbox)("OPENCODE_SERVER_PASSWORD"); !tc.noSandbox && got != tc.password {
				t.Fatalf("warning sees password %q, want %q", got, tc.password)
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
			want: `OMAC_SERVE_PASSWORD only works with the native sandbox backend, and profile "nono" uses another one. ` +
				"Unset OMAC_SERVE_PASSWORD, or set OPENCODE_SERVER_PASSWORD yourself where that backend runs the server.",
		},
		{
			name: "denied",
			plan: sandboxPlan{Name: "builtin", Native: true, PolicyRef: "strict", Policy: &sandboxprofile.Profile{Environment: sandboxprofile.Environment{DenyVars: []string{"OPENCODE_*"}}}},
			want: `OMAC_SERVE_PASSWORD is passed to the server as OPENCODE_SERVER_PASSWORD, but profile "strict" blocks that variable in environment.deny_vars. ` +
				"Remove the matching entry or unset OMAC_SERVE_PASSWORD.",
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
			hint := serverExposureWarning(h, inner, serveAuthGetenv(h, plan, extra, noSandbox))
			if (hint == "") != (password != "") {
				t.Errorf("noSandbox=%v password=%q: hint = %q", noSandbox, password, hint)
			}
		}
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
