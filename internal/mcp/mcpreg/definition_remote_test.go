package mcpreg

import (
	"strings"
	"testing"
)

func TestParseRemoteDefinition(t *testing.T) {
	ok := `
name: jira
secrets: [JIRA_TOKEN]
server:
  url: https://mcp.example.com/mcp
  bearer_secret: JIRA_TOKEN
  headers:
    X-Team: platform
`
	def, err := ParseDefinition([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if !def.Server.Remote() || def.Server.BearerSecret != "JIRA_TOKEN" || def.Server.Headers["X-Team"] != "platform" {
		t.Fatalf("def = %+v", def.Server)
	}
	if _, err := ParseDefinition([]byte("name: pub\nserver:\n  url: http://localhost:9000/mcp\n")); err != nil {
		t.Fatalf("a public loopback server without a token: %v", err)
	}
}

func TestRemoteDefinitionRefusals(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"command and url":      {"name: a\nserver:\n  command: [x]\n  url: https://e.com/mcp\n", "not both"},
		"plain http":           {"name: a\nserver:\n  url: http://e.com/mcp\n", "loopback"},
		"token in query":       {"name: a\nserver:\n  url: https://e.com/mcp?token=abc\n", "query"},
		"credential header":    {"name: a\nserver:\n  url: https://e.com/mcp\n  headers:\n    X-Api-Key: abc\n", "credential"},
		"authorization header": {"name: a\nserver:\n  url: https://e.com/mcp\n  headers:\n    Authorization: Bearer abc\n", "credential"},
		"reserved header":      {"name: a\nserver:\n  url: https://e.com/mcp\n  headers:\n    Mcp-Session-Id: x\n", "not allowed"},
		"newline in header":    {"name: a\nserver:\n  url: https://e.com/mcp\n  headers:\n    X-A: \"a\\nb\"\n", "control character"},
		"env with url":         {"name: a\nserver:\n  url: https://e.com/mcp\n  env:\n    A: b\n", "server.command only"},
		"bearer not declared":  {"name: a\nserver:\n  url: https://e.com/mcp\n  bearer_secret: T\n", "declared in secrets"},
		"extra secret":         {"name: a\nsecrets: [T, U]\nserver:\n  url: https://e.com/mcp\n  bearer_secret: T\n", "not used"},
		"command with headers": {"name: a\nserver:\n  command: [x]\n  headers:\n    X-A: b\n", "server.url only"},
		"neither":              {"name: a\nserver: {}\n", "server.command must not be empty"},
	}
	for name, c := range cases {
		_, err := ParseDefinition([]byte(c.yaml))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want mention of %q", name, err, c.want)
		}
	}
}
