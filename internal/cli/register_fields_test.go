package cli

import (
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

// canonicalizeFieldValue is the type-checking layer between user input
// (prompt, --fields-from file, OMAC_CONFIG_<NAME> env var) and the
// JSON store. The behavioral contract is:
//
//   string  -- regex-validated if Pattern set; preserved verbatim.
//   bool    -- accepts the human spellings; canonical form is "true"/"false".
//   int     -- accepts base-10; canonical form is the strconv rendering.
//   enum    -- exact match against Choices; preserved verbatim on hit.
//
// These tests pin the contract so prompt + non-interactive paths stay
// in sync.

func TestCanonicalize_String(t *testing.T) {
	spec := config.ConfigSpec{Name: "F", Type: config.ConfigFieldString}
	got, err := canonicalizeFieldValue(spec, "hello world")
	if err != nil || got != "hello world" {
		t.Errorf("string: got %q,%v; want %q,nil", got, err, "hello world")
	}
}

func TestCanonicalize_StringPattern(t *testing.T) {
	spec := config.ConfigSpec{Name: "F", Type: config.ConfigFieldString, Pattern: `^[a-z]+$`}
	if _, err := canonicalizeFieldValue(spec, "hello"); err != nil {
		t.Errorf("matching: unexpected err %v", err)
	}
	if _, err := canonicalizeFieldValue(spec, "Hello"); err == nil {
		t.Error("non-matching: expected error")
	}
}

func TestCanonicalize_Bool(t *testing.T) {
	spec := config.ConfigSpec{Name: "F", Type: config.ConfigFieldBool}
	for _, in := range []string{"yes", "Y", "1", "TRUE", "on"} {
		got, err := canonicalizeFieldValue(spec, in)
		if err != nil || got != "true" {
			t.Errorf("bool(%q) = %q,%v; want \"true\",nil", in, got, err)
		}
	}
	for _, in := range []string{"no", "N", "0", "FALSE", "off"} {
		got, err := canonicalizeFieldValue(spec, in)
		if err != nil || got != "false" {
			t.Errorf("bool(%q) = %q,%v; want \"false\",nil", in, got, err)
		}
	}
	if _, err := canonicalizeFieldValue(spec, "maybe"); err == nil {
		t.Error("bool(maybe): expected error")
	}
}

func TestCanonicalize_Int(t *testing.T) {
	spec := config.ConfigSpec{Name: "F", Type: config.ConfigFieldInt}
	for _, c := range []struct{ in, want string }{
		{"42", "42"},
		{"-7", "-7"},
		{"  100 ", "100"}, // whitespace tolerated
		{"0", "0"},
	} {
		got, err := canonicalizeFieldValue(spec, c.in)
		if err != nil || got != c.want {
			t.Errorf("int(%q) = %q,%v; want %q,nil", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"twelve", "1.5", "0x10", ""} {
		if _, err := canonicalizeFieldValue(spec, in); err == nil {
			t.Errorf("int(%q): expected error", in)
		}
	}
}

func TestCanonicalize_Enum(t *testing.T) {
	spec := config.ConfigSpec{
		Name: "F", Type: config.ConfigFieldEnum,
		Choices: []string{"alpha", "bravo", "charlie"},
	}
	if got, err := canonicalizeFieldValue(spec, "bravo"); err != nil || got != "bravo" {
		t.Errorf("enum hit: got %q,%v", got, err)
	}
	_, err := canonicalizeFieldValue(spec, "delta")
	if err == nil {
		t.Fatal("enum miss: expected error")
	}
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "charlie") {
		t.Errorf("enum miss error %q should list every choice", err)
	}
}

func TestCanonicalize_DefaultStringType(t *testing.T) {
	// Empty .Type should behave as string.
	spec := config.ConfigSpec{Name: "F"}
	got, err := canonicalizeFieldValue(spec, "anything goes")
	if err != nil || got != "anything goes" {
		t.Errorf("default-type: got %q,%v", got, err)
	}
}
