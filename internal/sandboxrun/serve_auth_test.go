package sandboxrun

import (
	"slices"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestServeAuthFinalFilter(t *testing.T) {
	environ := []string{"OMAC_SERVE_AUTH_TARGET=OPENCODE_SERVER_PASSWORD", "OPENCODE_SERVER_PASSWORD=dedicated"}
	for _, tc := range []struct {
		name    string
		policy  sandboxprofile.Environment
		wantErr string
	}{
		{
			name:   "allowed",
			policy: sandboxprofile.Environment{AllowVars: []string{"OPENCODE_SERVER_PASSWORD"}},
		},
		{
			name:    "denied",
			policy:  sandboxprofile.Environment{AllowVars: []string{"OPENCODE_SERVER_PASSWORD"}, DenyVars: []string{"OPENCODE_*"}},
			wantErr: sandboxprofile.ServeAuthDeniedError("OPENCODE_SERVER_PASSWORD", "default").Error(),
		},
		{
			name:   "not allowed",
			policy: sandboxprofile.Environment{AllowVars: []string{"PATH"}},
			wantErr: "OMAC_SERVE_PASSWORD cannot reach the server: the sandbox filtered out OPENCODE_SERVER_PASSWORD. " +
				`Check environment.allow_vars and environment.deny_vars in sandbox profile "default", or unset OMAC_SERVE_PASSWORD`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, err := filterServeAuthEnv(environ, tc.policy, nil, "default")
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Contains(env, "OPENCODE_SERVER_PASSWORD=dedicated") {
				t.Error("password missing")
			}
			if slices.Contains(env, "OMAC_SERVE_AUTH_TARGET=OPENCODE_SERVER_PASSWORD") {
				t.Error("internal marker leaked")
			}
		})
	}
}
