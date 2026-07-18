package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
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

// writeProfileFixture writes a default sandbox profile at its resolved path
// under the isolated HOME; callers isolateHome(t) first.
func writeProfileFixture(t *testing.T, json string) {
	t.Helper()
	path, err := sandboxprofile.ProfilePath("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnoseSurfacesBlockedHostAndHint(t *testing.T) {
	isolateHome(t)
	// A profile with an unused allow entry, so there is one advisory to
	// collapse alongside the blocked-host problem.
	writeProfileFixture(t, `{"meta":{"name":"default"},"network":{"mode":"filtered","allow_domain":["unused.example"],"network_prompt":{"enabled":false}}}`)
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
	if !strings.Contains(s, "2/2 connection(s) blocked") {
		t.Fatalf("blocked count not shown in status line:\n%s", s)
	}
	if !strings.Contains(s, "What to look at:") || !strings.Contains(s, "is not in any allow rule") {
		t.Fatalf("actionable problem not surfaced:\n%s", s)
	}
	// The dead-rule note (example.com unused) is an advisory: collapsed, not
	// dumped inline — keeping the default view focused.
	if strings.Contains(s, "matched no traffic") {
		t.Fatalf("advisory should be collapsed by default, not shown inline:\n%s", s)
	}
	if !strings.Contains(s, "advisory note(s)") {
		t.Fatalf("advisory collapse summary missing:\n%s", s)
	}
}

func TestDiagnoseVerboseExpandsAdvisoriesAndConfig(t *testing.T) {
	isolateHome(t)
	writeAuditFixture(t,
		`{"ts":"2026-07-18T10:00:00Z","run_id":"r1","type":"session.start"}`,
		`{"ts":"2026-07-18T10:00:01Z","run_id":"r1","type":"net.decision","host":"registry.npmjs.org","port":443,"allow":false,"source":"allowlist"}`,
	)
	env, out, _, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	runDiagnose([]string{"-v"}, env)
	drain()
	s := out.String()
	if !strings.Contains(s, "Effective network policy") {
		t.Fatalf("-v should show the effective config:\n%s", s)
	}
	if !strings.Contains(s, "audit trail:") {
		t.Fatalf("-v should show log paths:\n%s", s)
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
	s := out.String()
	if !strings.Contains(s, "HTTP(S) via proxy: DENY") || !strings.Contains(s, "hard-deny") {
		t.Fatalf("hard-deny probe should DENY the proxy path:\n%s", s)
	}
	if !strings.Contains(s, "raw TCP") {
		t.Fatalf("remote probe should also report the raw-TCP path:\n%s", s)
	}
}

// A raw-TCP tool (SSH, DB) uses allow_tcp_connect, not the proxy. The probe
// must report that path independently so `git@host` (SSH:22) is diagnosable.
func TestDiagnoseProbeRawTCP(t *testing.T) {
	isolateHome(t)
	writeProfileFixture(t, `{"meta":{"name":"default"},"network":{"mode":"filtered","allow_tcp_connect":[22]}}`)
	env, out, _, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	code := runDiagnose([]string{"--probe", "github.com:22"}, env)
	drain()
	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	s := out.String()
	if !strings.Contains(s, "raw TCP (SSH/DB):  ALLOW") {
		t.Fatalf("port 22 in allow_tcp_connect should ALLOW the raw-TCP path:\n%s", s)
	}
}

// Safety gate: --live on a non-ALLOW host must NOT launch a sandbox or dial;
// it reports the static outcome and a "skipped" live result. On the built-in
// default profile an unlisted host resolves to PROMPT, so no network happens.
func TestDiagnoseLiveProbeSkipsNonAllowedHost(t *testing.T) {
	isolateHome(t)
	env, out, _, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	code := runDiagnose([]string{"--probe", "unlisted.example.org", "--live", "--json"}, env)
	drain()

	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	var got probeView
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if got.Outcome == "ALLOW" {
		t.Fatalf("test premise broken: unlisted host should not be ALLOW")
	}
	if got.Live == nil || got.Live.Class != "skipped" {
		t.Fatalf("live probe must be skipped for a non-ALLOW host, got %+v", got.Live)
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
	if !strings.Contains(out.String(), "HTTP(S) via proxy: PROMPT") {
		t.Fatalf("unlisted host on default profile should report PROMPT on the proxy path:\n%s", out.String())
	}
}
