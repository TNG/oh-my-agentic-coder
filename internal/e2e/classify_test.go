package e2e

import (
	"strings"
	"testing"
)

func TestClassifyAgentOutputIncludesProbeSections(t *testing.T) {
	output := "=== PROBE: sidecar ===\nomac: unauthorized (send X-Omac-Facade-Token from $OMAC_FACADE_TOKEN)\n=== END: sidecar ===\n" +
		"=== PROBE: xskill ===\ncurl: (7) Failed to connect\n=== END: xskill ===\n"
	got := classifyAgentOutput(output)
	if !strings.Contains(got, "omac: unauthorized") {
		t.Errorf("sidecar probe body missing from classification:\n%s", got)
	}
	if !strings.Contains(got, "curl: (7)") {
		t.Errorf("xskill probe body missing from classification:\n%s", got)
	}

	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("x")
	}
	long := "=== PROBE: net ===\n" + b.String() + "\n=== END: net ===\n"
	if got := classifyAgentOutput(long); !strings.Contains(got, "…(truncated)") {
		t.Errorf("long probe section not truncated:\n%s", got)
	}

	if got := classifyAgentOutput("agent said nothing useful"); strings.Contains(got, "--- probe") {
		t.Errorf("absent probes should not render sections:\n%s", got)
	}
}
