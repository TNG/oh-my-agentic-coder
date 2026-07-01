package manifest

import (
	"strings"
	"testing"
)

func TestRenderEmptySkills(t *testing.T) {
	got := Render(`{"dir":"/proj","state":"active","skills":[]}`, ".claude/skills")
	if !strings.Contains(got, "omac skills available in this workspace") {
		t.Errorf("missing header, got:\n%s", got)
	}
	if strings.Contains(got, "- **") {
		t.Errorf("empty skills should have no skill lines, got:\n%s", got)
	}
}

func TestRenderReadySkill(t *testing.T) {
	input := `{"dir":"/proj","state":"active","skills":[
		{"name":"slack","scope":"workdir","state":"ready","base":"http://127.0.0.1:9100/slack"}
	]}`
	got := Render(input, ".claude/skills")
	if !strings.Contains(got, "- **slack** (workdir) — ready — base: `http://127.0.0.1:9100/slack`") {
		t.Errorf("missing ready skill line, got:\n%s", got)
	}
}

func TestRenderPendingCredentialsSkill(t *testing.T) {
	input := `{"dir":"/proj","state":"active","skills":[
		{"name":"slack","scope":"workdir","state":"pending-credentials","missing":["SLACK_TOKEN"]}
	]}`
	got := Render(input, ".claude/skills")
	if !strings.Contains(got, "UNAVAILABLE (missing credentials: SLACK_TOKEN)") {
		t.Errorf("missing pending-credentials line, got:\n%s", got)
	}
	if !strings.Contains(got, "omac secrets set slack SLACK_TOKEN") {
		t.Errorf("missing secrets hint, got:\n%s", got)
	}
}

func TestRenderBrokenSkill(t *testing.T) {
	input := `{"dir":"/proj","state":"active","skills":[
		{"name":"slack","scope":"workdir","state":"broken","detail":"connection refused"}
	]}`
	got := Render(input, ".claude/skills")
	if !strings.Contains(got, "BROKEN: connection refused") {
		t.Errorf("missing broken skill line, got:\n%s", got)
	}
}

func TestRenderGlobalSkillTargetPathWarning(t *testing.T) {
	input := `{"dir":"/proj","state":"active","skills":[
		{"name":"marketplace","scope":"global","state":"ready","base":"http://127.0.0.1:9100/marketplace"}
	]}`
	got := Render(input, ".claude/skills")
	if !strings.Contains(got, "global") {
		t.Errorf("missing global warning, got:\n%s", got)
	}
	if !strings.Contains(got, ".claude/skills") {
		t.Errorf("missing skills dir in target_path warning, got:\n%s", got)
	}
}

func TestRenderSkillsDirInParameter(t *testing.T) {
	// skillsDir appears in the global-ready target_path warning. With a global
	// ready skill present, the output must reference the skills dir parameter.
	input := `{"dir":"/proj","state":"active","skills":[
		{"name":"marketplace","scope":"global","state":"ready","base":"http://127.0.0.1:9100/marketplace"}
	]}`
	got := Render(input, ".codex/skills")
	if !strings.Contains(got, ".codex/skills") {
		t.Errorf("missing skills dir from parameter, got:\n%s", got)
	}
}
