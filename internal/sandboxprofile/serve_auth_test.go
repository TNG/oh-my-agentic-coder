package sandboxprofile

import "testing"

func TestServePasswordSourceNeverReachesSandbox(t *testing.T) {
	for _, allow := range [][]string{DefaultAllowVars(), {"OMAC_*"}, {"*"}, {"OMAC_SERVE_PASSWORD"}} {
		for _, injected := range []map[string]string{nil, {"OMAC_SERVE_PASSWORD": "injected-secret", "OMAC_SERVE_AUTH_TARGET": "OPENCODE_SERVER_PASSWORD"}} {
			got := envMap(FilterEnv([]string{"OMAC_SERVE_PASSWORD=source-secret", "OMAC_SERVE_AUTH_TARGET=OPENCODE_SERVER_PASSWORD", "OMAC_BASE=http://localhost"}, allow, nil, injected))
			for _, key := range []string{"OMAC_SERVE_PASSWORD", "OMAC_SERVE_AUTH_TARGET"} {
				if _, leaked := got[key]; leaked {
					t.Errorf("host-only %s survived the child environment filter", key)
				}
			}
		}
	}
}
