package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestServePassword(t *testing.T) {
	h, _ := config.LookupHarness("opencode")
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
