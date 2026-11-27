package cli

import (
	"strings"
	"testing"
)

func TestDefinitionFromURLArgs(t *testing.T) {
	def, err := definitionFromURLArgs("jira", "https://mcp.example.com/mcp", "JIRA_TOKEN", []string{"X-Team: platform"}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if def.Server.URL != "https://mcp.example.com/mcp" || def.Server.BearerSecret != "JIRA_TOKEN" || !def.Server.AllowPrivateNetwork ||
		def.Server.Headers["X-Team"] != "platform" || len(def.Secrets) != 1 || def.Secrets[0] != "JIRA_TOKEN" {
		t.Fatalf("def = %+v", def)
	}
}

func TestDefinitionFromURLArgsRefusals(t *testing.T) {
	cases := map[string]struct {
		headers []string
		secrets []string
		want    string
	}{
		"malformed header":     {headers: []string{"X-Team"}, want: "Name: value"},
		"variable reference":   {headers: []string{"X-A: ${TOKEN}"}, want: "variable reference"},
		"duplicate header":     {headers: []string{"X-A: 1", "X-A: 2"}, want: "twice"},
		"secret for a command": {secrets: []string{"T"}, want: "--bearer-secret"},
	}
	for name, c := range cases {
		_, err := definitionFromURLArgs("a", "https://e.com/mcp", "", c.headers, false, c.secrets)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want mention of %q", name, err, c.want)
		}
	}
}
