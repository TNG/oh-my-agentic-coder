package sandboxprofile

import (
	"strings"
	"testing"
)

func TestServeVariablesAreHostOnly(t *testing.T) {
	for _, vars := range [][]string{nil, {"*"}} {
		got := FilterEnv([]string{"OMAC_SERVE_PASSWORD=secret", "OMAC_SERVE_AUTH_TARGET=target"}, vars, nil, map[string]string{"OMAC_SERVE_PASSWORD": "injected", "OMAC_SERVE_AUTH_TARGET": "target"})
		for _, kv := range got {
			if strings.HasPrefix(kv, "OMAC_SERVE_") {
				t.Fatal("host-only serve variable leaked")
			}
		}
	}
}

// EnvDenied must agree with FilterEnv: a name it reports as denied is
// dropped even when allowed and injected, and any other name is kept.
func TestEnvDeniedMatchesFilterEnv(t *testing.T) {
	const name = "OPENCODE_SERVER_PASSWORD"
	for _, deny := range [][]string{nil, {"OTHER"}, {name}, {"OPENCODE_*"}, {"*"}} {
		kept := len(FilterEnv(nil, []string{"*"}, deny, map[string]string{name: "x"})) == 1
		if EnvDenied(name, deny) == kept {
			t.Errorf("deny %v: EnvDenied = %v, but FilterEnv kept = %v", deny, EnvDenied(name, deny), kept)
		}
	}
}
