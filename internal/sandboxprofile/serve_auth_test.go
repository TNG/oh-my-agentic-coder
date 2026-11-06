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
