package approval

import "fmt"

// Entry is one declared endpoint.
type Entry struct {
	Match       string
	Approval    Mode
	Description string
	catchAll    bool
}

// IsCatchAll reports whether the entry, as returned by Set.Lookup, is on ANY
// or a `**` path.
func (e Entry) IsCatchAll() bool { return e.catchAll }

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
// same literal/{name}/** layout, literals compared ignoring case), which
// would otherwise be ambiguous: a sidecar that ignores case cannot tell
// "GET /files/Public" from "GET /files/public" either.
// No entries is valid: nothing matches, so every request asks.
func Compile(entries []Entry) (*Set, error) {
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
			return nil, fmt.Errorf("approvals.endpoints[%d] (%s) has the same specificity as %s (letter case does not count)", i, e.Match, prev)
		}
		seen[p.shapeKey()] = e.Match
		e.catchAll = p.IsCatchAll()
		s.items = append(s.items, compiled{pat: p, entry: e})
	}
	return s, nil
}

// Lookup returns the most specific declared entry that admits the request.
// method is the upper-case request method; segs comes from CleanPath. ok is
// false when nothing is declared for the request; the returned entry then
// says `approval: required`, so an unlisted route asks however the result
// is read.
//
// Many sidecars route without regard to case (Express does by default), so
// "/files/SECRET" may run the handler of "/files/secret". When the best match
// says `none` but the most specific entry matching the path ignoring case is
// `required`, the request asks.
func (s *Set) Lookup(method string, segs []string) (Entry, bool) {
	best := s.mostSpecific(method, segs, Pattern.matches)
	if best == nil {
		return Entry{Approval: Required}, false
	}
	if best.entry.Approval == None {
		if strict := s.mostSpecific(method, segs, Pattern.matchesIgnoringCase); strict != nil && strict.entry.Approval == Required {
			return strict.entry, true
		}
	}
	return best.entry, true
}

// mostSpecific returns the most specific entry that match admits, or nil.
func (s *Set) mostSpecific(method string, segs []string, match func(Pattern, string, []string) bool) *compiled {
	var best *compiled
	for i := range s.items {
		c := &s.items[i]
		if !match(c.pat, method, segs) {
			continue
		}
		if best == nil || c.pat.moreSpecific(best.pat) {
			best = c
		}
	}
	return best
}

// AskAll returns a Set that declares nothing, so every lookup misses and
// every request asks.
func AskAll() *Set { return &Set{} }
