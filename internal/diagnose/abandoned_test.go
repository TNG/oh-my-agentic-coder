package diagnose

import (
	"strings"
	"testing"
)

func filtered() Policy { return Policy{Mode: "filtered", PromptEnabled: true} }

// TestAbandonedPromptReported is the #257 headline: an abandoned prompt must
// surface as a problem, not vanish because no verdict exists.
func TestAbandonedPromptReported(t *testing.T) {
	abandoned := []AbandonedPrompt{{Host: "models.dev", Port: 443, WaitedMS: 4800}}
	r := Build(filtered(), nil, abandoned, realMatch)

	if len(r.Abandoned) != 1 {
		t.Fatalf("Report.Abandoned = %v, want 1 entry", r.Abandoned)
	}
	h := findHint(r.Hints, "never answered in time")
	if h == nil {
		t.Fatalf("no abandoned-prompt hint; hints = %+v", r.Hints)
	}
	if h.Kind != KindProblem {
		t.Errorf("hint kind = %q, want %q", h.Kind, KindProblem)
	}
	joined := h.Title + " " + strings.Join(h.Detail, " ")
	if !strings.Contains(joined, "models.dev:443") {
		t.Errorf("hint does not name the host: %q", joined)
	}
	if !strings.Contains(joined, "4.8s") {
		t.Errorf("hint does not report how long the tool waited: %q", joined)
	}
	if !strings.Contains(joined, "network.allow_domain") {
		t.Errorf("hint does not name the remedy: %q", joined)
	}
}

// TestAbandonedPromptSuppressesNothingBlockedNote is the regression guard for
// the actual misreport in #257: with an abandoned prompt on record, diagnose
// must not also claim nothing was blocked.
func TestAbandonedPromptSuppressesNothingBlockedNote(t *testing.T) {
	allowed := []Decision{{Host: "registry.npmjs.org", Port: 443, Allowed: true, Source: "prompt"}}

	withAbandoned := Build(filtered(), allowed,
		[]AbandonedPrompt{{Host: "models.dev", Port: 443, WaitedMS: 5000}}, realMatch)
	if h := findHint(withAbandoned.Hints, "can be invisible to this report"); h != nil {
		t.Errorf("still emitted the 'nothing was blocked' note alongside an abandoned prompt: %q", h.Title)
	}

	// Control: without an abandoned prompt the note is still the right call.
	clean := Build(filtered(), allowed, nil, realMatch)
	if h := findHint(clean.Hints, "can be invisible to this report"); h == nil {
		t.Error("expected the 'nothing was blocked' note for an all-allowed run")
	}
}

func TestNoAbandonedPromptsNoHint(t *testing.T) {
	r := Build(filtered(), []Decision{{Host: "a.test", Allowed: true}}, nil, realMatch)
	if len(r.Abandoned) != 0 {
		t.Errorf("Report.Abandoned = %v, want empty", r.Abandoned)
	}
	if h := findHint(r.Hints, "never answered in time"); h != nil {
		t.Errorf("emitted an abandoned-prompt hint with none recorded: %q", h.Title)
	}
}

func TestWaitedForUnits(t *testing.T) {
	for _, tt := range []struct {
		ms   int64
		want string
	}{
		{0, "0ms"},
		{999, "999ms"},
		{1000, "1.0s"},
		{4800, "4.8s"},
	} {
		if got := waitedFor(tt.ms); got != tt.want {
			t.Errorf("waitedFor(%d) = %q, want %q", tt.ms, got, tt.want)
		}
	}
}

func TestHostPortOmitsZeroPort(t *testing.T) {
	if got := hostPort("a.test", 0); got != "a.test" {
		t.Errorf("hostPort with no port = %q, want %q", got, "a.test")
	}
	if got := hostPort("a.test", 443); got != "a.test:443" {
		t.Errorf("hostPort = %q, want a.test:443", got)
	}
}
