// Package hostmap resolves an egress hostname to the harness subsystem that
// dials it, so the network prompt can show a human-readable "likely cause".
//
// The data is harness-specific (see opencode-egress.json); loaders are keyed
// by harness via For, and a harness with no map yields a nil *Map whose Lookup
// always misses. The proxy never terminates TLS, so the hostname is the only
// signal available at prompt time — this map is what turns it into a cause.
package hostmap

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed opencode-egress.json
var opencodeEgress []byte

//go:embed claude-code-egress.json
var claudeCodeEgress []byte

// Entry is one host→cause mapping. The whole entry is returned (not just the
// cause string) so callers that also know the originating process can honour
// the process-dependent disambiguation recorded in Notes.
type Entry struct {
	Host          string `json:"host"`
	Match         string `json:"match"` // "exact" | "suffix"
	Origin        string `json:"origin"`
	Category      string `json:"category"`
	Cause         string `json:"cause"`
	UserInitiated bool   `json:"user_initiated"`
	Source        string `json:"source"`
	Notes         string `json:"notes"`
}

// Map indexes egress entries for one harness.
type Map struct {
	exact  map[string]Entry
	suffix []Entry
}

type file struct {
	Entries []Entry `json:"entries"`
}

// For returns the egress map for a harness, or nil when none is known for it.
// A nil *Map is safe to use: its Lookup always returns (Entry{}, false), so
// callers need no nil check.
//
// harness must be a CANONICAL harness name (e.g. "opencode", "claude-code"),
// as resolved through config.LookupHarness by the caller — not a binary
// basename or alias ("claude"). Keying on the canonical name keeps harness
// identity single-sourced in the config registry.
func For(harness string) *Map {
	var raw []byte
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "opencode":
		raw = opencodeEgress
	case "claude-code":
		raw = claudeCodeEgress
	default:
		return nil
	}
	m, err := parse(raw)
	if err != nil {
		return nil
	}
	return m
}

func parse(raw []byte) (*Map, error) {
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("hostmap: parse: %w", err)
	}
	m := &Map{exact: make(map[string]Entry)}
	for _, e := range f.Entries {
		h := normalize(e.Host)
		if h == "" || e.Cause == "" {
			continue
		}
		if e.Match == "suffix" {
			m.suffix = append(m.suffix, e)
			continue
		}
		m.exact[h] = e
	}
	return m, nil
}

// Lookup returns the entry for a host: an exact match first, then any suffix
// entry that the host equals or is a subdomain of. Nil-safe.
func (m *Map) Lookup(host string) (Entry, bool) {
	if m == nil {
		return Entry{}, false
	}
	h := normalize(host)
	if h == "" {
		return Entry{}, false
	}
	if e, ok := m.exact[h]; ok {
		return e, true
	}
	for _, e := range m.suffix {
		s := normalize(e.Host)
		if h == s || strings.HasSuffix(h, "."+s) {
			return e, true
		}
	}
	return Entry{}, false
}

func normalize(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}
