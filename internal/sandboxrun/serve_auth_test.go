package sandboxrun

import (
	"slices"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestServeAuthFinalFilter(t *testing.T) {
	for _, deny := range [][]string{nil, {"OPENCODE_*"}} {
		env, err := filterServeAuthEnv([]string{"OMAC_SERVE_AUTH_TARGET=OPENCODE_SERVER_PASSWORD", "OPENCODE_SERVER_PASSWORD=dedicated"}, sandboxprofile.Environment{AllowVars: []string{"OPENCODE_SERVER_PASSWORD"}, DenyVars: deny}, nil)
		if (err != nil) != (len(deny) > 0) {
			t.Fatalf("deny %v: %v", deny, err)
		}
		if err == nil && !slices.Contains(env, "OPENCODE_SERVER_PASSWORD=dedicated") {
			t.Fatal("password missing")
		}
		for _, kv := range env {
			if kv == "OMAC_SERVE_AUTH_TARGET=OPENCODE_SERVER_PASSWORD" {
				t.Fatal("internal marker leaked")
			}
		}
	}
}
