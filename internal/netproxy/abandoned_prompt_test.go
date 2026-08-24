package netproxy

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
)

// eventsOfType filters a capturingAuditor's record by event type.
func eventsOfType(c *capturingAuditor, typ string) []audit.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []audit.Event
	for _, ev := range c.events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// stubResolve pins any host to a routable address so Check reaches the
// prompt stage instead of failing DNS.
func stubResolve(context.Context, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

// TestDrainAbandonedPromptsEmitsEventForOpenPrompt is the #257 core: a prompt
// still open at shutdown must leave a record, because no net.decision will
// ever be written for it.
func TestDrainAbandonedPromptsEmitsEventForOpenPrompt(t *testing.T) {
	rec := &capturingAuditor{}
	prompter := &blockingPrompter{block: make(chan struct{}), res: PromptResult{Allow: true}}
	f := NewFilter(FilterConfig{
		PromptEnabled: true,
		Prompter:      prompter,
		Auditor:       rec,
		Resolve:       stubResolve,
	})

	// A request that reaches the prompt and never gets an answer.
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Check(context.Background(), "models.dev", 443)
	}()
	waitUntil(t, func() bool { return prompter.started.Load() == 1 })

	if n := f.DrainAbandonedPrompts(); n != 1 {
		t.Fatalf("drained %d prompt(s), want 1", n)
	}
	evs := eventsOfType(rec, audit.TypeNetPromptAbandoned)
	if len(evs) != 1 {
		t.Fatalf("emitted %d abandoned event(s), want 1", len(evs))
	}
	if evs[0].Host != "models.dev" || evs[0].Port != 443 {
		t.Errorf("event = %s:%d, want models.dev:443", evs[0].Host, evs[0].Port)
	}

	// Release the parked prompt so the goroutine can finish.
	close(prompter.block)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt goroutine did not finish")
	}
}

// TestDrainAbandonedPromptsIgnoresAnsweredPrompt asserts a prompt that WAS
// answered produces a net.decision and no abandoned event — otherwise every
// normal run would report a phantom failure.
func TestDrainAbandonedPromptsIgnoresAnsweredPrompt(t *testing.T) {
	rec := &capturingAuditor{}
	f := NewFilter(FilterConfig{
		PromptEnabled: true,
		Prompter:      &fakePrompter{res: PromptResult{Allow: true}},
		Auditor:       rec,
		Resolve:       stubResolve,
	})

	if v, _ := f.Check(context.Background(), "example.test", 443); v.Decision != Allow {
		t.Fatalf("verdict = %v, want Allow", v.Decision)
	}
	if n := f.DrainAbandonedPrompts(); n != 0 {
		t.Errorf("drained %d prompt(s) after an answered prompt, want 0", n)
	}
	if evs := eventsOfType(rec, audit.TypeNetPromptAbandoned); len(evs) != 0 {
		t.Errorf("emitted %d abandoned event(s) for an answered prompt, want 0", len(evs))
	}
	if evs := eventsOfType(rec, audit.TypeNetDecision); len(evs) != 1 {
		t.Errorf("emitted %d decision(s), want 1", len(evs))
	}
}

// TestPromptWaitRecordedOnDecision covers the other half of #257: the prompt
// IS answered, but late. A decision exists (so nothing looks wrong) and only
// the recorded wait reveals that a short-timeout client had already failed.
func TestPromptWaitRecordedOnDecision(t *testing.T) {
	rec := &capturingAuditor{}
	prompter := &blockingPrompter{block: make(chan struct{}), res: PromptResult{Allow: true}}
	f := NewFilter(FilterConfig{
		PromptEnabled: true,
		Prompter:      prompter,
		Auditor:       rec,
		Resolve:       stubResolve,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Check(context.Background(), "models.dev", 443)
	}()
	waitUntil(t, func() bool { return prompter.started.Load() == 1 })
	time.Sleep(30 * time.Millisecond) // let the dialog be "open" measurably
	close(prompter.block)
	<-done

	evs := eventsOfType(rec, audit.TypeNetDecision)
	if len(evs) != 1 {
		t.Fatalf("emitted %d decision(s), want 1", len(evs))
	}
	if evs[0].WaitedMS <= 0 {
		t.Errorf("decision WaitedMS = %d, want > 0 for a prompt-sourced decision", evs[0].WaitedMS)
	}
}

// TestNonPromptDecisionHasNoWait keeps the new field out of decisions that
// never involved a dialog, so a diagnose threshold cannot false-positive.
func TestNonPromptDecisionHasNoWait(t *testing.T) {
	rec := &capturingAuditor{}
	f := NewFilter(FilterConfig{
		AllowDomains: []string{"allowed.test"},
		Auditor:      rec,
		Resolve:      stubResolve,
	})
	f.Check(context.Background(), "allowed.test", 443)

	evs := eventsOfType(rec, audit.TypeNetDecision)
	if len(evs) != 1 {
		t.Fatalf("emitted %d decision(s), want 1", len(evs))
	}
	if evs[0].WaitedMS != 0 {
		t.Errorf("WaitedMS = %d for an allowlist decision, want 0", evs[0].WaitedMS)
	}
}

// TestDrainedPromptAnsweredLateEmitsNoDecision guards against one request
// being reported twice with contradictory outcomes: once as abandoned, then
// again as an ordinary allow when the dialog is finally answered.
func TestDrainedPromptAnsweredLateEmitsNoDecision(t *testing.T) {
	rec := &capturingAuditor{}
	prompter := &blockingPrompter{block: make(chan struct{}), res: PromptResult{Allow: true}}
	f := NewFilter(FilterConfig{
		PromptEnabled: true,
		Prompter:      prompter,
		Auditor:       rec,
		Resolve:       stubResolve,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Check(context.Background(), "models.dev", 443)
	}()
	waitUntil(t, func() bool { return prompter.started.Load() == 1 })

	if n := f.DrainAbandonedPrompts(); n != 1 {
		t.Fatalf("drained %d, want 1", n)
	}
	// The user answers after the drain — too late to matter.
	close(prompter.block)
	<-done

	if evs := eventsOfType(rec, audit.TypeNetPromptAbandoned); len(evs) != 1 {
		t.Errorf("abandoned events = %d, want 1", len(evs))
	}
	if evs := eventsOfType(rec, audit.TypeNetDecision); len(evs) != 0 {
		t.Errorf("emitted %d decision(s) for an already-abandoned prompt, want 0", len(evs))
	}
}

func TestDrainAbandonedPromptsIsIdempotent(t *testing.T) {
	rec := &capturingAuditor{}
	f := NewFilter(FilterConfig{Auditor: rec})
	if n := f.DrainAbandonedPrompts(); n != 0 {
		t.Errorf("drained %d with no prompts in flight, want 0", n)
	}
	if n := f.DrainAbandonedPrompts(); n != 0 {
		t.Errorf("second drain returned %d, want 0", n)
	}
	if evs := eventsOfType(rec, audit.TypeNetPromptAbandoned); len(evs) != 0 {
		t.Errorf("emitted %d event(s) with nothing in flight", len(evs))
	}
}
