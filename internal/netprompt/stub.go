package netprompt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// --- decision source ---

// stubDecision is one resolved decision for a host.
type stubDecision struct {
	Allow       bool   // true = allow, false = deny
	Persist     bool   // permanent vs once
	Scope       string // "host" or "suffix" when Persist
	NeedsIntent bool   // deny with "explain more" marker
}

// decisionSource resolves a decision for a given host. The file-based
// source reads a JSON file; a future socket-based source can query a
// control socket — both implement this interface.
type decisionSource interface {
	lookup(host string) (stubDecision, bool)
}

// fileDecisionSource reads per-host decisions from a JSON file.
// Format: {"host.example": {"allow": true, "persist": true, "scope": "suffix"}, ...}
// A wildcard key "*" matches any host not otherwise listed.
type fileDecisionSource struct {
	mu        sync.RWMutex
	path      string
	loaded    bool
	decisions map[string]stubDecision
	logf      func(string, ...any)
}

func newFileDecisionSource(path string, logf func(string, ...any)) *fileDecisionSource {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &fileDecisionSource{path: path, logf: logf}
}

func (f *fileDecisionSource) lookup(host string) (stubDecision, bool) {
	f.mu.RLock()
	if !f.loaded {
		f.mu.RUnlock()
		f.load()
		f.mu.RLock()
	}
	defer f.mu.RUnlock()
	host = strings.ToLower(host)
	if d, ok := f.decisions[host]; ok {
		return d, true
	}
	if d, ok := f.decisions["*"]; ok {
		return d, true
	}
	return stubDecision{}, false
}

func (f *fileDecisionSource) load() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loaded {
		return
	}
	f.decisions = map[string]stubDecision{}
	data, err := os.ReadFile(f.path)
	if err != nil {
		// A missing/unreadable file is a real misconfiguration: without it
		// every host falls through to "Deny once" with no signal. Surface
		// it rather than silently denying.
		f.logf("omac sandbox: stub prompt: cannot read decisions file %q: %v; denying every host", f.path, err)
		f.loaded = true
		return
	}
	if err := json.Unmarshal(data, &f.decisions); err != nil {
		f.logf("omac sandbox: stub prompt: malformed decisions file %q: %v; denying every host", f.path, err)
	}
	f.loaded = true
}

// --- stub backend ---

// stubBackend is a dialogBackend that reads decisions from a
// decisionSource instead of showing a GUI dialog. Activated by
// OMAC_PROMPT_STUB=1; the decisions file path is
// OMAC_PROMPT_DECISIONS.
//
// The stub maps a stubDecision to the same option labels the native
// backends use (labelToToken / tokenToResult handle the rest):
//
//	allow + !persist  → "Allow once"
//	allow + persist + scope=host   → "Allow permanently (this host)"
//	allow + persist + scope=suffix → "Allow permanently (*.suffix)"
//	deny + !persist + !needsIntent → "Deny once"
//	deny + persist + scope=host    → "Deny permanently (this host)"
//	deny + persist + scope=suffix  → "Deny permanently (*.suffix)"
//	deny + needsIntent              → "Explain more"
//
// When no decision is on file for a host, the stub returns "Deny once"
// (conservative default — same as a user cancelling the dialog).
type stubBackend struct {
	source decisionSource
	logf   func(string, ...any)
}

func (stubBackend) name() string { return "stub" }

func (s stubBackend) available() bool { return s.source != nil }

func (s stubBackend) show(ctx context.Context, host string, port int, suffix, intent, cause string) (string, error) {
	d, ok := s.source.lookup(host)
	if !ok {
		s.logf("omac sandbox: stub prompt: no decision for %s; denying", host)
		return "Deny once", nil
	}
	label := decisionToLabel(d, suffix)
	s.logf("omac sandbox: stub prompt: %s → %s (intent: %q)", host, label, intent)
	return label, nil
}

// decisionToLabel maps a stubDecision to the option label the prompter
// expects (mirrors optionLabels / labelToToken).
func decisionToLabel(d stubDecision, suffix string) string {
	switch {
	case d.Allow && d.Persist && d.Scope == "host":
		return "Allow permanently (this host)"
	case d.Allow && d.Persist && d.Scope == "suffix":
		return fmt.Sprintf("Allow permanently (*.%s)", suffix)
	case d.Allow:
		return "Allow once"
	case d.NeedsIntent:
		return "Explain more"
	case !d.Allow && d.Persist && d.Scope == "host":
		return "Deny permanently (this host)"
	case !d.Allow && d.Persist && d.Scope == "suffix":
		return fmt.Sprintf("Deny permanently (*.%s)", suffix)
	default:
		return "Deny once"
	}
}
