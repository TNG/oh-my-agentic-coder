package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyProbeError(t *testing.T) {
	cases := []struct {
		msg       string
		wantClass string
		wantReach bool
	}{
		{"proxyconnect: 403 Forbidden", "blocked", false},
		{`Head "https://x/": dial tcp: lookup x: no such host`, "dns", false},
		{"dial tcp 1.2.3.4:443: connect: connection refused", "refused", false},
		{"context deadline exceeded (Client.Timeout)", "timeout", false},
		{"remote error: tls: handshake failure", "tls", true},
		{"some unmapped failure", "error", false},
	}
	for _, c := range cases {
		got := classifyProbeError("h:443", errors.New(c.msg))
		if got.Class != c.wantClass {
			t.Errorf("%q -> class %q, want %q", c.msg, got.Class, c.wantClass)
		}
		if got.Reached != c.wantReach {
			t.Errorf("%q -> reached %v, want %v", c.msg, got.Reached, c.wantReach)
		}
	}
}

// The inner command must write its result to --out and keep stdout empty, so
// the parent's --json output stays parseable. A refused local port keeps the
// probe off the external network.
func TestProbeConnectWritesResultFileAndKeepsStdoutClean(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "res.json")
	env, out, _, drain := newPipeEnv(t, "")
	code := runSandboxProbeConnect([]string{"--out", outPath, "--timeout", "2", "127.0.0.1:1"}, env)
	drain()

	if code != ExitOK {
		t.Fatalf("code=%d, want ExitOK", code)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout must be empty when --out is set, got: %q", out.String())
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("result file not written: %v", err)
	}
	var res probeResult
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("result not valid JSON: %v\n%s", err, data)
	}
	if res.Reached {
		t.Fatalf("connection to a refused local port must not report reached: %+v", res)
	}
	if res.Class == "" {
		t.Fatalf("result missing class: %+v", res)
	}
}

func TestProbeConnectHumanOutputWhenNoOutFile(t *testing.T) {
	env, out, _, drain := newPipeEnv(t, "")
	code := runSandboxProbeConnect([]string{"--timeout", "2", "127.0.0.1:1"}, env)
	drain()

	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.HasPrefix(out.String(), "probe 127.0.0.1:1:") {
		t.Fatalf("want human line on stdout without --out, got: %q", out.String())
	}
}
