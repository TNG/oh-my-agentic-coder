// Package netprompt implements the interactive network prompt for the
// built-in sandbox and the learned-policy persistence behind the
// "permanently" choices. The file format is byte-compatible with
// nono's learned-policy files so existing decisions migrate by copy:
//
//	{"schema":1,"entries":[{"host":"...","scope":"host"|"suffix",
//	                        "decision":"allow"|"deny"}]}
package netprompt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// learnedSchema is the only supported schema version.
const learnedSchema = 1

// LearnedEntry is one persisted decision.
type LearnedEntry struct {
	Host     string `json:"host"`
	Scope    string `json:"scope"`    // "host" | "suffix"
	Decision string `json:"decision"` // "allow" | "deny"
}

type learnedFile struct {
	Schema  int            `json:"schema"`
	Entries []LearnedEntry `json:"entries"`
}

// LearnedPolicy is a thread-safe learned-decision store backed by a
// JSON file written atomically on every change. It implements
// netproxy.LearnedStore.
type LearnedPolicy struct {
	mu      sync.Mutex
	path    string
	entries []LearnedEntry
}

// LoadLearnedPolicy reads the file at path (missing file = empty store).
func LoadLearnedPolicy(path string) (*LearnedPolicy, error) {
	lp := &LearnedPolicy{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return lp, nil
		}
		return nil, fmt.Errorf("read learned policy %s: %w", path, err)
	}
	var f learnedFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse learned policy %s: %w", path, err)
	}
	if f.Schema != learnedSchema {
		return nil, fmt.Errorf("learned policy %s: unsupported schema %d", path, f.Schema)
	}
	lp.entries = f.Entries
	return lp, nil
}

// Lookup implements netproxy.LearnedStore. Deny entries win over allow
// entries; suffix entries match the host itself and any subdomain.
func (lp *LearnedPolicy) Lookup(host string) (allow bool, found bool) {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	h := strings.ToLower(host)
	var match *LearnedEntry
	for i := range lp.entries {
		e := &lp.entries[i]
		if !entryMatches(e, h) {
			continue
		}
		if e.Decision == "deny" {
			return false, true // deny wins immediately
		}
		match = e
	}
	if match != nil {
		return match.Decision == "allow", true
	}
	return false, false
}

// Entries returns a copy of the learned decisions for display.
func (lp *LearnedPolicy) Entries() []LearnedEntry {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	return append([]LearnedEntry(nil), lp.entries...)
}

func entryMatches(e *LearnedEntry, host string) bool {
	target := strings.ToLower(e.Host)
	if e.Scope == "suffix" {
		return host == target || strings.HasSuffix(host, "."+target)
	}
	return host == target
}

// Record implements netproxy.LearnedStore: upserts and persists
// atomically (temp file + rename).
func (lp *LearnedPolicy) Record(host, scope string, allow bool) error {
	if scope != "host" && scope != "suffix" {
		return fmt.Errorf("invalid learned scope %q", scope)
	}
	decision := "deny"
	if allow {
		decision = "allow"
	}
	lp.mu.Lock()
	defer lp.mu.Unlock()
	h := strings.ToLower(host)
	replaced := false
	for i := range lp.entries {
		if strings.ToLower(lp.entries[i].Host) == h && lp.entries[i].Scope == scope {
			lp.entries[i].Decision = decision
			replaced = true
			break
		}
	}
	if !replaced {
		lp.entries = append(lp.entries, LearnedEntry{Host: h, Scope: scope, Decision: decision})
	}
	return lp.saveLocked()
}

func (lp *LearnedPolicy) saveLocked() error {
	if lp.path == "" {
		return nil // in-memory only
	}
	if err := os.MkdirAll(filepath.Dir(lp.path), 0o755); err != nil {
		return fmt.Errorf("create learned policy dir: %w", err)
	}
	data, err := json.MarshalIndent(learnedFile{Schema: learnedSchema, Entries: lp.entries}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(lp.path), ".learned-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, lp.path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
