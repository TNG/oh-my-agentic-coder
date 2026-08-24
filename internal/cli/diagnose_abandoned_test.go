package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDiagnoseSurfacesAbandonedPrompt reproduces the run shape from #257: the
// only network activity was an allowed fetch plus a prompt the requesting tool
// stopped waiting for. Before this, diagnose reported "0/1 blocked · nothing
// was blocked" for a run that had silently failed.
func TestDiagnoseSurfacesAbandonedPrompt(t *testing.T) {
	isolateHome(t)
	writeProfileFixture(t, `{"meta":{"name":"default"},"network":{"mode":"filtered","network_prompt":{"enabled":true}}}`)
	writeAuditFixture(t,
		`{"ts":"2026-08-24T09:58:11Z","run_id":"r1","type":"session.start"}`,
		`{"ts":"2026-08-24T09:58:12Z","run_id":"r1","type":"net.decision","host":"registry.npmjs.org","port":443,"allow":true,"source":"allowlist"}`,
		`{"ts":"2026-08-24T09:58:16Z","run_id":"r1","type":"net.prompt_abandoned","host":"models.dev","port":443,"waited_ms":4800}`,
		`{"ts":"2026-08-24T09:58:17Z","run_id":"r1","type":"session.stop","exit_code":0}`,
	)

	env, out, _, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	if code := runDiagnose(nil, env); code != ExitOK {
		t.Fatalf("code=%d, want ExitOK", code)
	}
	drain()
	s := out.String()

	if !strings.Contains(s, "1 prompt(s) abandoned") {
		t.Errorf("status line does not carry the abandoned count:\n%s", s)
	}
	if !strings.Contains(s, "models.dev:443") {
		t.Errorf("abandoned host not named:\n%s", s)
	}
	if !strings.Contains(s, "4.8s") {
		t.Errorf("time the tool waited not reported:\n%s", s)
	}
	// The core regression: the reassuring note must be gone.
	if strings.Contains(s, "Nothing was blocked by the network policy") {
		t.Errorf("still reported 'nothing was blocked' for a run with an abandoned prompt:\n%s", s)
	}
	if strings.Contains(s, "No problems detected") {
		t.Errorf("still reported 'no problems detected':\n%s", s)
	}
}

// TestDiagnoseJSONCarriesAbandonedPrompts keeps the machine-readable view in
// step with the text one, so automation can detect this class too.
func TestDiagnoseJSONCarriesAbandonedPrompts(t *testing.T) {
	isolateHome(t)
	writeProfileFixture(t, `{"meta":{"name":"default"},"network":{"mode":"filtered","network_prompt":{"enabled":true}}}`)
	writeAuditFixture(t,
		`{"ts":"2026-08-24T09:58:11Z","run_id":"r1","type":"session.start"}`,
		`{"ts":"2026-08-24T09:58:16Z","run_id":"r1","type":"net.prompt_abandoned","host":"models.dev","port":443,"waited_ms":4800}`,
	)

	env, out, _, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	if code := runDiagnose([]string{"--json"}, env); code != ExitOK {
		t.Fatalf("code=%d, want ExitOK", code)
	}
	drain()

	var view struct {
		Report struct {
			Abandoned []struct {
				Host     string `json:"host"`
				Port     int    `json:"port"`
				WaitedMS int64  `json:"waited_ms"`
			} `json:"abandoned"`
		} `json:"report"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("decode --json output: %v\n%s", err, out.String())
	}
	if len(view.Report.Abandoned) != 1 {
		t.Fatalf("report.abandoned = %+v, want 1 entry", view.Report.Abandoned)
	}
	got := view.Report.Abandoned[0]
	if got.Host != "models.dev" || got.Port != 443 || got.WaitedMS != 4800 {
		t.Errorf("abandoned entry = %+v, want models.dev:443 waited 4800ms", got)
	}
}

// TestDiagnoseUnaffectedWithoutAbandonedPrompts guards against the new axis
// leaking into ordinary runs.
func TestDiagnoseUnaffectedWithoutAbandonedPrompts(t *testing.T) {
	isolateHome(t)
	writeProfileFixture(t, `{"meta":{"name":"default"},"network":{"mode":"filtered","network_prompt":{"enabled":true}}}`)
	writeAuditFixture(t,
		`{"ts":"2026-08-24T09:58:11Z","run_id":"r1","type":"session.start"}`,
		`{"ts":"2026-08-24T09:58:12Z","run_id":"r1","type":"net.decision","host":"registry.npmjs.org","port":443,"allow":true,"source":"allowlist"}`,
	)

	env, out, _, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	runDiagnose(nil, env)
	drain()
	s := out.String()

	if strings.Contains(s, "abandoned") {
		t.Errorf("mentioned abandoned prompts for a run with none:\n%s", s)
	}
	if !strings.Contains(s, "Nothing was blocked by the network policy") {
		t.Errorf("expected the all-allowed note to still fire:\n%s", s)
	}
}
