package approval

import "fmt"

// Entry is one declared endpoint.
type Entry struct {
	Match       string
	Approval    Mode
	Description string
}

type compiled struct {
	pat   Pattern
	entry Entry
}

// Set is a compiled, validated list of declared endpoints.
type Set struct {
	items []compiled
}

// Compile validates the entries and returns a Set. It fails on an unparsable
// match, a value other than required/none, `approval: none` on a catch-all
// (ANY or a `**` path), and two entries of the same shape (same method and
// same literal/{name}/** layout), which would otherwise be ambiguous.
func Compile(entries []Entry) (*Set, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("approvals.endpoints must declare at least one endpoint")
	}
	seen := map[string]string{}
	s := &Set{}
	for i, e := range entries {
		p, err := ParsePattern(e.Match)
		if err != nil {
			return nil, fmt.Errorf("approvals.endpoints[%d]: %w", i, err)
		}
		if !e.Approval.Valid() {
			return nil, fmt.Errorf("approvals.endpoints[%d] (%s): approval must be %q or %q, got %q", i, e.Match, Required, None, e.Approval)
		}
		if e.Approval == None && p.IsCatchAll() {
			return nil, fmt.Errorf("approvals.endpoints[%d] (%s): approval %q is not allowed on ANY or a ** path; a catch-all must be explicit and %q", i, e.Match, None, Required)
		}
		if prev, dup := seen[p.shapeKey()]; dup {
			return nil, fmt.Errorf("approvals.endpoints[%d] (%s) has the same specificity as %s", i, e.Match, prev)
		}
		seen[p.shapeKey()] = e.Match
		s.items = append(s.items, compiled{pat: p, entry: e})
	}
	return s, nil
}

// Lookup returns the most specific declared entry that admits the request.
// method is the upper-case request method; segs comes from CleanPath. ok is
// false when nothing is declared for the request: the caller rejects it.
func (s *Set) Lookup(method string, segs []string) (Entry, bool) {
	var best *compiled
	for i := range s.items {
		c := &s.items[i]
		if !c.pat.matches(method, segs) {
			continue
		}
		if best == nil || c.pat.moreSpecific(best.pat) {
			best = c
		}
	}
	if best == nil {
		return Entry{}, false
	}
	return best.entry, true
}

// DenyAll returns a Set that declares nothing, so every lookup misses.
func DenyAll() *Set { return &Set{} }
