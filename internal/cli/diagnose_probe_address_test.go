package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestDiagnoseProbeAddressClassification(t *testing.T) {
	for _, tc := range []struct {
		host     string
		loopback bool
	}{
		{"169.254.169.254", false},
		{"fe80::1%0", false},
		{"::ffff:169.254.169.254", false},
		{"64:ff9b::169.254.169.254%0", false},
		{"64:ff9b:1::169.254.169.254%lo0", false},
		{"fd00:ec2::254%0", false},
		{"127.0.0.1", true},
		{"::1%0", true},
		{"0.0.0.0", true},
		{"::", true},
		{"::ffff:127.0.0.1", true},
		{"64:ff9b::127.0.0.1%0", true},
		{"64:ff9b:1::0.0.0.0%lo0", true},
		{"localhost.", true},
		{"tool.localhost", true},
		{"0x7f.0.0.1", true},
	} {
		for _, grant := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/grant=%v", tc.host, grant), func(t *testing.T) {
				isolateHome(t)
				ports := "[]"
				if grant {
					ports = "[80]"
				}
				writeProfileFixture(t, fmt.Sprintf(`{"meta":{"name":"default"},"network":{"mode":"filtered","open_port":%s,"allow_domain":["other.example"],"network_prompt":{"enabled":false}}}`, ports))
				env, out, _, drain := newPipeEnv(t, "")
				env.Workdir = t.TempDir()
				code := runDiagnose([]string{"--json", "--probe", net.JoinHostPort(tc.host, "80")}, env)
				drain()
				var got probeView
				if code != ExitOK || json.Unmarshal(out.Bytes(), &got) != nil {
					t.Fatalf("diagnose failed: %d %s", code, out.String())
				}
				want := "DENY"
				if tc.loopback && grant {
					want = "ALLOW"
				}
				if got.Outcome != want || (got.RawTCP == nil) != tc.loopback {
					t.Fatalf("wrong address classification: %s", out.String())
				}
				if !tc.loopback && (!strings.HasPrefix(got.Reason, "hard-deny") || strings.Contains(got.Reason, "open_port")) {
					t.Fatalf("non-loopback target must use the hard-deny policy, not suggest a loopback grant: %s", out.String())
				}
			})
		}
	}
}
