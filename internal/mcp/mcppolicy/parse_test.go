package mcppolicy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRequestToolCallRoundTrip(t *testing.T) {
	body := []byte(` { "params" : { "arguments" : {"q": "a < b & c", "n": 1.50}, "name":"get" },
	  "method":"tools/call", "id": 7, "jsonrpc":"2.0" } `)
	call, canonical, err := ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if call.Method != "tools/call" || call.Tool != "get" || string(call.ID) != "7" || call.Response {
		t.Fatalf("call = %+v", call)
	}
	if string(call.Args) != `{"q": "a < b & c", "n": 1.50}` {
		t.Errorf("args = %s", call.Args)
	}
	want := `{"id":7,"jsonrpc":"2.0","method":"tools/call","params":{"arguments":{"q":"a < b & c","n":1.50},"name":"get"}}`
	if string(canonical) != want {
		t.Errorf("canonical = %s\nwant      %s", canonical, want)
	}
	again, canonical2, err := ParseRequest(canonical)
	if err != nil || string(canonical2) != string(canonical) || again.Tool != "get" {
		t.Errorf("canonical form is not stable: %v %s", err, canonical2)
	}
}

func TestParseRequestOtherMessages(t *testing.T) {
	cases := []struct {
		name, body, method string
		id                 string
		response           bool
	}{
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "notifications/initialized", "", false},
		{"string id", `{"jsonrpc":"2.0","id":"abc","method":"tools/list"}`, "tools/list", `"abc"`, false},
		{"null params", `{"jsonrpc":"2.0","id":1,"method":"ping","params":null}`, "ping", "1", false},
		{"resource method still parses", `{"id":1,"method":"resources/read","params":{"uri":"x"}}`, "resources/read", "1", false},
		{"response", `{"jsonrpc":"2.0","id":3,"result":{}}`, "", "3", true},
		{"error response", `{"jsonrpc":"2.0","id":"x","error":{"code":1,"message":"m"}}`, "", `"x"`, true},
	}
	for _, tc := range cases {
		call, canonical, err := ParseRequest([]byte(tc.body))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if call.Method != tc.method || string(call.ID) != tc.id || call.Response != tc.response || len(canonical) == 0 {
			t.Errorf("%s: %+v", tc.name, call)
		}
	}
}

func TestParseRequestRefusals(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 100) + "1" + strings.Repeat("}", 100)
	cases := map[string]string{
		"batch":                      `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		"empty":                      ``,
		"whitespace only":            "  \n",
		"string":                     `"tools/call"`,
		"number":                     `1`,
		"null":                       `null`,
		"invalid json":               `{"method":`,
		"trailing value":             `{"method":"ping","id":1} {"method":"ping","id":2}`,
		"trailing garbage":           `{"method":"ping","id":1} x`,
		"duplicate method":           `{"method":"ping","method":"tools/call","id":1}`,
		"duplicate id":               `{"method":"ping","id":1,"id":2}`,
		"duplicate params":           `{"method":"tools/call","id":1,"params":{"name":"a"},"params":{"name":"b"}}`,
		"duplicate name":             `{"method":"tools/call","id":1,"params":{"name":"get","name":"rm"}}`,
		"duplicate arguments":        `{"method":"tools/call","id":1,"params":{"name":"get","arguments":{},"arguments":{"x":1}}}`,
		"duplicate nested in args":   `{"method":"tools/call","id":1,"params":{"name":"get","arguments":{"a":{"b":1,"b":2}}}}`,
		"duplicate in array element": `{"method":"ping","id":1,"params":{"x":[{"k":1,"k":2}]}}`,
		"escaped duplicate":          `{"method":"ping","id":1,"params":{},"params":{}}`,
		"case variant Method":        `{"Method":"tools/call","method":"ping","id":1}`,
		"case variant only Method":   `{"Method":"ping","id":1}`,
		"case variant ID":            `{"method":"ping","id":1,"ID":2}`,
		"case variant Params":        `{"method":"tools/call","id":1,"PARAMS":{"name":"rm"},"params":{"name":"get"}}`,
		"case variant Name":          `{"method":"tools/call","id":1,"params":{"name":"get","Name":"rm"}}`,
		"case variant Arguments":     `{"method":"tools/call","id":1,"params":{"name":"get","Arguments":{}}}`,
		"no method":                  `{"id":1}`,
		"method not string":          `{"method":5,"id":1}`,
		"empty method":               `{"method":"","id":1}`,
		"id object":                  `{"method":"ping","id":{}}`,
		"id bool":                    `{"method":"ping","id":true}`,
		"tools/call without params":  `{"method":"tools/call","id":1}`,
		"tools/call null params":     `{"method":"tools/call","id":1,"params":null}`,
		"tools/call array params":    `{"method":"tools/call","id":1,"params":["get"]}`,
		"tools/call without name":    `{"method":"tools/call","id":1,"params":{"arguments":{}}}`,
		"name number":                `{"method":"tools/call","id":1,"params":{"name":1}}`,
		"name null":                  `{"method":"tools/call","id":1,"params":{"name":null}}`,
		"name array":                 `{"method":"tools/call","id":1,"params":{"name":["get"]}}`,
		"name object":                `{"method":"tools/call","id":1,"params":{"name":{"v":"get"}}}`,
		"name empty":                 `{"method":"tools/call","id":1,"params":{"name":""}}`,
		"name lone surrogate escape": `{"method":"tools/call","id":1,"params":{"name":"a\ud800"}}`,
		"arguments array":            `{"method":"tools/call","id":1,"params":{"name":"get","arguments":[]}}`,
		"arguments string":           `{"method":"tools/call","id":1,"params":{"name":"get","arguments":"x"}}`,
		"too deep":                   `{"method":"ping","id":1,"params":` + deep + `}`,
		"invalid utf8":               "{\"method\":\"tools/call\",\"id\":1,\"params\":{\"name\":\"a\xffb\"}}",
		"unterminated nested":        `{"method":"ping","id":1,"params":{"a":[1,2}}`,
	}
	for name, body := range cases {
		if _, canonical, err := ParseRequest([]byte(body)); err == nil {
			t.Errorf("%s: accepted, canonical %s", name, canonical)
		}
	}
}

func TestParseRequestCaseVariantInArgumentsIsFine(t *testing.T) {
	body := `{"method":"tools/call","id":1,"params":{"name":"get","arguments":{"Name":"a","name":"b","ID":1}}}`
	call, _, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var args map[string]any
	if err := json.Unmarshal(call.Args, &args); err != nil || len(args) != 3 {
		t.Errorf("arguments must pass through untouched: %v %v", err, args)
	}
}

func TestParseRequestArgumentsNullIsAbsent(t *testing.T) {
	call, _, err := ParseRequest([]byte(`{"method":"tools/call","id":1,"params":{"name":"get","arguments":null}}`))
	if err != nil || call.Args != nil {
		t.Errorf("err=%v args=%s", err, call.Args)
	}
}
