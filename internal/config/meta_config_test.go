package config

import (
	"strings"
	"testing"
)

// validMeta returns a Meta with the smallest valid sidecar block,
// suitable for adding ConfigSpecs onto. Tests append to .Sidecar.Config
// or .Sidecar.Secrets and call Validate().
func validMeta() *Meta {
	return &Meta{
		Name: "test-skill",
		Sidecar: &SidecarMeta{
			Command: []string{"true"},
		},
	}
}

func TestSidecarConfig_Valid_AllTypes(t *testing.T) {
	m := validMeta()
	m.Sidecar.Config = []ConfigSpec{
		{Name: "STR_FIELD", Type: ConfigFieldString, Pattern: `^[a-z]+$`, Default: "hello"},
		{Name: "BOOL_FIELD", Type: ConfigFieldBool, Default: "yes"},
		{Name: "INT_FIELD", Type: ConfigFieldInt, Default: "42"},
		{Name: "ENUM_FIELD", Type: ConfigFieldEnum, Choices: []string{"a", "b", "c"}, Default: "b"},
		{Name: "DEFAULT_STRING_TYPE"}, // empty type defaults to string
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: unexpected error: %v", err)
	}
}

func TestSidecarConfig_RejectsInvalidName(t *testing.T) {
	m := validMeta()
	m.Sidecar.Config = []ConfigSpec{{Name: "lowercase"}}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected validation error for lowercase field name")
	}
	if !strings.Contains(err.Error(), "valid env var name") {
		t.Errorf("error %q should mention env var name validity", err)
	}
}

func TestSidecarConfig_RejectsUnknownType(t *testing.T) {
	m := validMeta()
	m.Sidecar.Config = []ConfigSpec{{Name: "FIELD", Type: "json"}}
	err := m.Validate()
	if err == nil {
		t.Fatal("expected validation error for unknown type")
	}
	if !strings.Contains(err.Error(), "string, bool, int, enum") {
		t.Errorf("error should list allowed types: %v", err)
	}
}

func TestSidecarConfig_EnumWithoutChoices(t *testing.T) {
	m := validMeta()
	m.Sidecar.Config = []ConfigSpec{{Name: "F", Type: ConfigFieldEnum}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "non-empty 'choices'") {
		t.Fatalf("expected enum-without-choices error, got %v", err)
	}
}

func TestSidecarConfig_EnumDefaultMustBeInChoices(t *testing.T) {
	m := validMeta()
	m.Sidecar.Config = []ConfigSpec{{
		Name: "F", Type: ConfigFieldEnum,
		Choices: []string{"a", "b"}, Default: "z",
	}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "not in choices") {
		t.Fatalf("expected default-not-in-choices error, got %v", err)
	}
}

func TestSidecarConfig_BoolRejectsPattern(t *testing.T) {
	m := validMeta()
	m.Sidecar.Config = []ConfigSpec{{Name: "F", Type: ConfigFieldBool, Pattern: `^.*$`}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "type=bool") {
		t.Fatalf("expected bool-rejects-pattern error, got %v", err)
	}
}

func TestSidecarConfig_IntDefaultMustParse(t *testing.T) {
	m := validMeta()
	m.Sidecar.Config = []ConfigSpec{{Name: "F", Type: ConfigFieldInt, Default: "twelve"}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "not a valid integer") {
		t.Fatalf("expected int-default-parse error, got %v", err)
	}
}

func TestSidecarConfig_BoolDefaultMustParse(t *testing.T) {
	m := validMeta()
	m.Sidecar.Config = []ConfigSpec{{Name: "F", Type: ConfigFieldBool, Default: "maybe"}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "not a valid bool") {
		t.Fatalf("expected bool-default-parse error, got %v", err)
	}
}

// Collision tests: the env-var namespace is shared between secrets,
// config fields, and env_passthrough. Validate() must reject any pair
// that would race in supervisor.go's spawn-env construction.
func TestSidecarConfig_RejectsNameCollisionWithSecret(t *testing.T) {
	m := validMeta()
	m.Sidecar.Secrets = []SecretSpec{{Name: "SHARED"}}
	m.Sidecar.Config = []ConfigSpec{{Name: "SHARED"}}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "declared by both secrets and config") {
		t.Fatalf("expected secret/config collision error, got %v", err)
	}
}

// Config + env_passthrough collision is rejected: there's no semantic
// reason to have both, and the duplicate just confuses skill authors.
func TestSidecarConfig_RejectsNameCollisionWithEnvPassthrough(t *testing.T) {
	m := validMeta()
	m.Sidecar.Config = []ConfigSpec{{Name: "SHARED"}}
	m.Sidecar.EnvPassthrough = []string{"SHARED"}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "env_passthrough and config") {
		t.Fatalf("expected config/env_passthrough collision error, got %v", err)
	}
}

// secrets + env_passthrough collision is INTENTIONALLY allowed: the
// established convention (echo-rest) is to list a secret in
// env_passthrough as a fallback for environments where the keychain is
// unavailable. At runtime secrets always win over passthrough, so the
// duplicate is harmless and removing this allowance would break
// existing skills.
func TestSidecarSecrets_AllowsEnvPassthroughOverlap(t *testing.T) {
	m := validMeta()
	m.Sidecar.Secrets = []SecretSpec{{Name: "API_TOKEN"}}
	m.Sidecar.EnvPassthrough = []string{"API_TOKEN"}
	if err := m.Validate(); err != nil {
		t.Fatalf("secrets+env_passthrough overlap should be allowed: %v", err)
	}
}

func TestSidecarConfig_RejectsSelfCollision(t *testing.T) {
	m := validMeta()
	m.Sidecar.Config = []ConfigSpec{
		{Name: "DUP"},
		{Name: "DUP"},
	}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "declared by both") {
		t.Fatalf("expected duplicate-config-name error, got %v", err)
	}
}

func TestParseBoolField(t *testing.T) {
	cases := map[string]string{
		"true": "true", "True": "true", "TRUE": "true",
		"yes": "true", "y": "true", "1": "true", "on": "true",
		"false": "false", "False": "false", "FALSE": "false",
		"no": "false", "n": "false", "0": "false", "off": "false",
		" true ": "true", // whitespace tolerated
	}
	for in, want := range cases {
		got, err := ParseBoolField(in)
		if err != nil {
			t.Errorf("ParseBoolField(%q): unexpected err %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseBoolField(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "maybe", "trueish", "2"} {
		if _, err := ParseBoolField(bad); err == nil {
			t.Errorf("ParseBoolField(%q): expected error", bad)
		}
	}
}

func TestConfigSpec_EffectiveType_Default(t *testing.T) {
	c := ConfigSpec{Name: "F"}
	if c.EffectiveType() != ConfigFieldString {
		t.Errorf("default type should be string, got %q", c.EffectiveType())
	}
}

func TestConfigSpec_IsRequired_Default(t *testing.T) {
	c := ConfigSpec{Name: "F"}
	if !c.IsRequired() {
		t.Error("default should be required=true")
	}
	f := false
	c.Required = &f
	if c.IsRequired() {
		t.Error("Required=&false should yield IsRequired()==false")
	}
}
