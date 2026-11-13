package diagnose

import (
	"fmt"
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

// TestSlowPromptReported covers the answered-but-too-late half of #257: a
// decision exists and reads as "allowed", so only the recorded wait shows the
// requesting tool had already timed out.
func TestSlowPromptReported(t *testing.T) {
	decisions := []Decision{
		{Host: "models.dev", Port: 443, Allowed: true, Source: "prompt", WaitedMS: 5200},
	}
	r := Build(filtered(), decisions, nil, realMatch)

	h := findHint(r.Hints, "may have failed anyway")
	if h == nil {
		t.Fatalf("no slow-prompt hint; hints = %+v", r.Hints)
	}
	if h.Kind != KindProblem {
		t.Errorf("hint kind = %q, want %q", h.Kind, KindProblem)
	}
	joined := h.Title + " " + strings.Join(h.Detail, " ")
	if !strings.Contains(joined, "models.dev:443") || !strings.Contains(joined, "5.2s") {
		t.Errorf("hint does not name host and wait: %q", joined)
	}
	// It must also displace the reassuring note, which is the actual misreport.
	if n := findHint(r.Hints, "can be invisible to this report"); n != nil {
		t.Errorf("still emitted 'nothing was blocked' alongside a slow prompt: %q", n.Title)
	}
}

// TestFastPromptNotReported keeps the threshold from firing on ordinary
// pre-allowlisted or promptly answered traffic.
func TestFastPromptNotReported(t *testing.T) {
	for _, d := range []Decision{
		{Host: "a.test", Allowed: true, Source: "prompt", WaitedMS: 800},
		{Host: "b.test", Allowed: true, Source: "allowlist", WaitedMS: 0},
		// A non-prompt source must never be flagged even with a stale wait.
		{Host: "c.test", Allowed: true, Source: "allowlist", WaitedMS: 9000},
	} {
		r := Build(filtered(), []Decision{d}, nil, realMatch)
		if h := findHint(r.Hints, "may have failed anyway"); h != nil {
			t.Errorf("flagged %+v as a slow prompt: %q", d, h.Title)
		}
	}
}

// TestAbandonedPromptsAggregatedAndCapped guards the focused view: one line
// per host, capped, rather than one line per occurrence across the log.
func TestAbandonedPromptsAggregatedAndCapped(t *testing.T) {
	var abandoned []AbandonedPrompt
	// One host repeated many times, plus more distinct hosts than the cap.
	for i := 0; i < 5; i++ {
		abandoned = append(abandoned, AbandonedPrompt{Host: "noisy.test", Port: 443, WaitedMS: int64(1000 + i)})
	}
	for i := 0; i < maxPromptHostsShown+3; i++ {
		abandoned = append(abandoned, AbandonedPrompt{Host: fmt.Sprintf("h%d.test", i), Port: 443, WaitedMS: 2000})
	}

	h := findHint(Build(filtered(), nil, abandoned, realMatch).Hints, "never answered in time")
	if h == nil {
		t.Fatal("no abandoned-prompt hint")
	}
	noisyLines := 0
	for _, d := range h.Detail {
		if strings.Contains(d, "noisy.test") {
			noisyLines++
		}
	}
	if noisyLines != 1 {
		t.Errorf("noisy.test appears on %d detail line(s), want 1 aggregated line", noisyLines)
	}
	if !strings.Contains(strings.Join(h.Detail, " "), "×5") {
		t.Errorf("aggregated line does not report the repeat count: %v", h.Detail)
	}
	// Cap: host lines + the "and N more" line + the two closing lines.
	if len(h.Detail) > maxPromptHostsShown+3 {
		t.Errorf("detail has %d lines, want at most %d", len(h.Detail), maxPromptHostsShown+3)
	}
	if !strings.Contains(strings.Join(h.Detail, " "), "more host(s)") {
		t.Errorf("no truncation notice despite exceeding the cap: %v", h.Detail)
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
