package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
)

// writeAuditFixture writes a JSONL audit trail at the default path under the
// isolated HOME and returns nothing; callers isolateHome(t) first.
func writeAuditFixture(t *testing.T, lines ...string) {
	t.Helper()
	path, _ := audit.DefaultPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnoseSurfacesBlockedHostAndHint(t *testing.T) {
	isolateHome(t)
	writeAuditFixture(t,
		`{"ts":"2026-07-18T10:00:00Z","run_id":"r1","type":"session.start"}`,
		`{"ts":"2026-07-18T10:00:01Z","run_id":"r1","type":"net.decision","host":"registry.npmjs.org","port":443,"allow":false,"source":"allowlist"}`,
		`{"ts":"2026-07-18T10:00:02Z","run_id":"r1","type":"net.decision","host":"registry.npmjs.org","port":443,"allow":false,"source":"allowlist"}`,
	)

	env, out, _, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	code := runDiagnose(nil, env)
	drain()

	if code != ExitOK {
		t.Fatalf("code=%d, want ExitOK", code)
	}
	s := out.String()
	if !strings.Contains(s, "registry.npmjs.org") {
		t.Fatalf("blocked host not surfaced:\n%s", s)
	}
	if !strings.Contains(s, "2 denied") {
		t.Fatalf("denied count not shown:\n%s", s)
	}
	if !strings.Contains(s, "is not in any allow rule") {
		t.Fatalf("actionable hint missing:\n%s", s)
	}
	if !strings.Contains(s, "audit trail:") {
		t.Fatalf("audit log path not shown:\n%s", s)
	}
}

func TestDiagnoseNoTrailIsGraceful(t *testing.T) {
	isolateHome(t)
	env, out, _, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	code := runDiagnose(nil, env)
	drain()

	if code != ExitOK {
		t.Fatalf("code=%d, want ExitOK when no trail exists", code)
	}
	if !strings.Contains(out.String(), "no audit trail yet") {
		t.Fatalf("missing friendly no-trail note:\n%s", out.String())
	}
}

func TestDiagnoseJSONIsValid(t *testing.T) {
	isolateHome(t)
	writeAuditFixture(t,
		`{"ts":"2026-07-18T10:00:00Z","run_id":"r1","type":"session.start"}`,
		`{"ts":"2026-07-18T10:00:01Z","run_id":"r1","type":"net.decision","host":"a.example","port":443,"allow":false,"source":"allowlist"}`,
	)
	env, out, _, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	code := runDiagnose([]string{"--json"}, env)
	drain()

	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	var got struct {
		Report struct {
			Denied  int `json:"denied"`
			Blocked []struct {
				Host string `json:"Host"`
			} `json:"blocked"`
		} `json:"report"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if got.Report.Denied != 1 {
		t.Fatalf("want denied=1, got %d", got.Report.Denied)
	}
}

// Probe of a hard-deny host is definitive regardless of prompt config.
func TestDiagnoseProbeHardDeny(t *testing.T) {
	isolateHome(t)
	env, out, _, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	code := runDiagnose([]string{"--probe", "169.254.169.254"}, env)
	drain()

	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.HasPrefix(out.String(), "DENY 169.254.169.254") {
		t.Fatalf("hard-deny probe should DENY:\n%s", out.String())
	}
}

// On the default profile an unlisted host has no static rule and the prompt
// is enabled, so the probe reports the interactive-prompt outcome rather
// than a misleading allow/deny.
func TestDiagnoseProbeUnlistedHostReportsPrompt(t *testing.T) {
	isolateHome(t)
	env, out, _, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	code := runDiagnose([]string{"--probe", "example.com:443"}, env)
	drain()

	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	if !strings.HasPrefix(out.String(), "PROMPT example.com:443") {
		t.Fatalf("unlisted host on default profile should report PROMPT:\n%s", out.String())
	}
}
