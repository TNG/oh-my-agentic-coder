package mcpinject

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

const (
	envValue    = "ghp_SECRETENVVALUE"
	headerValue = "Bearer SECRETHEADERVALUE"
)

func TestAddCommandGolden(t *testing.T) {
	tests := []struct {
		name string
		e    Existing
		want string
	}{
		{"project claude with secrets", Existing{Harness: "claude-code", Name: "gh", Transport: "stdio", Command: "npx",
			Args: []string{"-y", "@mcp/server-github", "--root", "/my dir", "it's"}, EnvKeys: []string{"GITHUB_TOKEN", "LOG"}},
			`omac mcp add --scope project --harness claude-code --secret GITHUB_TOKEN --secret LOG gh -- npx -y @mcp/server-github --root '/my dir' 'it'\''s'`},
		{"user level opencode", Existing{Harness: "opencode", Name: "fs", UserLevel: true, Transport: "stdio", Command: "/usr/bin/fs"},
			`omac mcp add --scope global --harness opencode fs -- /usr/bin/fs`},
		{"remote", Existing{Harness: "claude-code", Name: "r", Transport: "http", HeaderKeys: []string{"Authorization"}}, "remote MCP servers are not supported yet"},
		{"control character", Existing{Harness: "opencode", Name: "x", Command: "a", Args: []string{"b\nc"}}, noAddCommand},
		{"no command", Existing{Harness: "opencode", Name: "x"}, noAddCommand},
	}
	for _, tt := range tests {
		if got := AddCommand(tt.e); got != tt.want {
			t.Errorf("%s:\n got %s\nwant %s", tt.name, got, tt.want)
		}
	}
}

func TestScanFillsEntryDetailsWithoutValues(t *testing.T) {
	home, workdir := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	write(t, filepath.Join(workdir, ".mcp.json"), `{"mcpServers": {
  "gh": {"command": "npx", "args": ["-y", "server"], "env": {"GITHUB_TOKEN": "`+envValue+`", "A": "b"}},
  "web": {"type": "http", "url": "https://h/mcp", "headers": {"Authorization": "`+headerValue+`"}}
}}`)
	write(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"user": {"type":"stdio","command":"x"}}}`)
	write(t, filepath.Join(workdir, "opencode.json"), `{"mcp": {
  "loc": {"type": "local", "command": ["node", "s.js", "--p"], "environment": {"TOKEN": "`+envValue+`"}},
  "rem": {"type": "remote", "url": "https://h", "headers": {"X-Key": "`+headerValue+`"}}
}}`)

	ex, _, err := Scan(config.MCPScanClaude, hostFor(workdir, home))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Existing{}
	for _, e := range ex {
		byName[e.Name] = e
	}
	gh := byName["gh"]
	if gh.Transport != "stdio" || gh.Command != "npx" || !reflect.DeepEqual(gh.Args, []string{"-y", "server"}) ||
		!reflect.DeepEqual(gh.EnvKeys, []string{"A", "GITHUB_TOKEN"}) || gh.UserLevel {
		t.Fatalf("gh = %+v", gh)
	}
	if web := byName["web"]; web.Transport != "http" || !reflect.DeepEqual(web.HeaderKeys, []string{"Authorization"}) {
		t.Fatalf("web = %+v", web)
	}
	if u := byName["user"]; !u.UserLevel || u.Transport != "stdio" {
		t.Fatalf("user = %+v", u)
	}

	ex, _, err = Scan(config.MCPScanOpenCode, hostFor(workdir, home))
	if err != nil {
		t.Fatal(err)
	}
	byName = map[string]Existing{}
	for _, e := range ex {
		byName[e.Name] = e
	}
	if loc := byName["loc"]; loc.Command != "node" || !reflect.DeepEqual(loc.Args, []string{"s.js", "--p"}) || !reflect.DeepEqual(loc.EnvKeys, []string{"TOKEN"}) {
		t.Fatalf("loc = %+v", loc)
	}
	if rem := byName["rem"]; rem.Transport != "remote" || !reflect.DeepEqual(rem.HeaderKeys, []string{"X-Key"}) {
		t.Fatalf("rem = %+v", rem)
	}
}

func TestEvaluateRefusalHintsHaveNoValues(t *testing.T) {
	home, workdir := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	write(t, filepath.Join(workdir, ".mcp.json"), `{"mcpServers": {
  "gh": {"command": "npx", "args": ["-y", "server"], "env": {"GITHUB_TOKEN": "`+envValue+`"}},
  "web": {"type": "http", "url": "https://h/mcp", "headers": {"Authorization": "`+headerValue+`"}}
}}`)
	write(t, filepath.Join(workdir, "opencode.json"), `{"mcp": {
  "loc": {"type": "local", "command": ["node", "s.js"], "environment": {"TOKEN": "`+envValue+`"}}
}}`)

	for _, tt := range []struct {
		kind config.MCPScanKind
		want []string
	}{
		{config.MCPScanClaude, []string{
			"omac mcp add --scope project --harness claude-code --secret GITHUB_TOKEN gh -- npx -y server",
			"remote MCP servers are not supported yet",
			"Check the copied arguments for tokens",
		}},
		{config.MCPScanOpenCode, []string{
			"omac mcp add --scope project --harness opencode --secret TOKEN loc -- node s.js",
			"Check the copied arguments for tokens",
		}},
	} {
		ex, _, err := Scan(tt.kind, hostFor(workdir, home))
		if err != nil {
			t.Fatal(err)
		}
		_, refusal := Evaluate(Policy{Unregistered: ModeRefuse, Active: true}, ex, nil)
		warnings, _ := Evaluate(Policy{Unregistered: ModeWarn, Active: true}, ex, nil)
		for label, text := range map[string]string{"refuse": refusal.Error(), "warn": strings.Join(warnings, "\n")} {
			for _, want := range tt.want {
				if !strings.Contains(text, want) {
					t.Errorf("%s: output lacks %q:\n%s", label, want, text)
				}
			}
			if strings.Contains(text, envValue) || strings.Contains(text, headerValue) {
				t.Errorf("%s: output holds an env or header value:\n%s", label, text)
			}
			for _, kept := range []string{"rotate", "omac mcp register", "--mcp-unregistered=allow"} {
				if label == "refuse" && !strings.Contains(text, kept) {
					t.Errorf("refusal lost %q", kept)
				}
			}
		}
	}
}
