package mcpreg

import (
	"reflect"
	"strings"
	"testing"
)

func TestFromServerEntry(t *testing.T) {
	def, err := FromServerEntry("demo", []byte(`{"type":"stdio","command":"/bin/x","args":["a","b"],"env":{"LEVEL":"info"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(def.Server.Command, []string{"/bin/x", "a", "b"}) || def.Server.Env["LEVEL"] != "info" || len(def.Secrets) != 0 {
		t.Fatalf("def = %+v", def)
	}
}

func TestFromServerEntryRefusals(t *testing.T) {
	tests := []struct{ json, want string }{
		{`{"url":"https://h"}`, "remote MCP servers are not supported yet"},
		{`{"command":"x","headers":{}}`, "remote MCP servers are not supported yet"},
		{`{"type":"streamable-http","command":"x"}`, "remote MCP servers are not supported yet"},
		{`{"command":"x","timeout":3}`, "unknown field"},
		{`{"args":["a"]}`, "command is required"},
		{`{"command":"x","env":{"A":""}}`, "declare --secret A"},
		{`{"command":"x","env":{"A":"pre-${B}"}}`, "declare --secret A"},
		{`[1]`, "JSON object"},
	}
	for _, tt := range tests {
		if _, err := FromServerEntry("demo", []byte(tt.json)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.json, err, tt.want)
		}
	}
}

func TestParseServerEntrySecrets(t *testing.T) {
	def, values, err := ParseServerEntry("demo", []byte(`{"command":"x","env":{"TOK":"abc","EMPTY":"","REF":"${REF}","LEVEL":"1"}}`), []string{"TOK", "EMPTY", "REF", "EXTRA"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(def.Secrets, []string{"EMPTY", "REF", "TOK", "EXTRA"}) {
		t.Fatalf("secrets = %v", def.Secrets)
	}
	if len(values) != 1 || values["TOK"].ExposeString() != "abc" {
		t.Fatalf("values = %v", values)
	}
	if _, ok := def.Server.Env["TOK"]; ok || len(def.Server.Env) != 1 {
		t.Fatalf("env = %v", def.Server.Env)
	}
}
