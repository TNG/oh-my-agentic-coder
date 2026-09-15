//go:build vuln

package cli

import (
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/skillconfig"
)

// TestSecurityRegisterOutputStripsControlSequences asserts that a config
// field's own name — which comes straight from the skill's omac.yaml, a
// file the skill author controls — cannot inject terminal control
// sequences into `omac register`'s prompt output.
//
// handleOneField writes spec.Name directly into a fmt.Fprintf with no
// escaping; visibleLen (style.go) only skips escape sequences for width
// alignment, never for output. A field name containing an ANSI CSI
// sequence can repaint or clear what the operator sees while approving a
// skill.
func TestSecurityRegisterOutputStripsControlSequences(t *testing.T) {
	env, _, errBuf, drain := newPipeEnv(t, "value\n")
	store := &skillconfig.Store{}
	payload := "harmless\x1b[2Jname"
	spec := config.ConfigSpec{Name: payload, Type: config.ConfigFieldString}

	if _, err := handleOneField(env, store, "skill", spec, false, nil, nil, false, nil); err != nil {
		t.Fatalf("handleOneField: %v", err)
	}
	drain()
	out := errBuf.String()

	// Control: the field name is actually rendered (this fixture reaches
	// the vulnerable code), so a fix that suppresses all prompt output
	// wouldn't make this test pass for the wrong reason.
	if !strings.Contains(out, "harmless") {
		t.Fatalf("control: the field name was not rendered at all: the fixture is broken, not the security property\n%q", out)
	}

	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("omac register prompt output contains a raw ANSI escape from a skill-supplied config field name: %q", out)
	}
}
