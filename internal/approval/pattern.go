package approval

import (
	"fmt"
	"regexp"
	"strings"
)

// Mode is the approval requirement of one declared endpoint.
type Mode string

const (
	// Required means the facade asks the user before forwarding.
	Required Mode = "required"
	// None means the facade forwards without asking.
	None Mode = "none"
)

// Valid reports whether m is one of the two declared values.
func (m Mode) Valid() bool { return m == Required || m == None }

// MethodAny is the method wildcard in a match pattern.
const MethodAny = "ANY"

var (
	methods = map[string]bool{
		"GET": true, "HEAD": true, "POST": true, "PUT": true,
		"PATCH": true, "DELETE": true, "OPTIONS": true,
	}
	paramRE   = regexp.MustCompile(`^\{[A-Za-z_][A-Za-z0-9_]*\}$`)
	literalRE = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

type segKind int

const (
	segLiteral segKind = iota
	segParam
	segRest
)

type segment struct {
	kind segKind
	lit  string
}

// Pattern is a parsed `<METHOD|ANY> <path>` match expression.
type Pattern struct {
	Method string // upper-case method or MethodAny
	segs   []segment
	raw    string
}

// String returns the pattern as written (normalized spacing).
func (p Pattern) String() string { return p.raw }

// ParsePattern parses `<METHOD|ANY> <path>`. The path is relative to the
// mount, starts with "/", and has literal segments, `{name}` (exactly one
// non-empty segment) and `**` (the rest of the path, zero or more segments,
// last only). There is no query part and no regular expression.
func ParsePattern(s string) (Pattern, error) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return Pattern{}, fmt.Errorf("match %q: want \"<METHOD|ANY> <path>\"", s)
	}
	method, path := fields[0], fields[1]
	if method != MethodAny && !methods[method] {
		return Pattern{}, fmt.Errorf("match %q: method %q is not one of GET HEAD POST PUT PATCH DELETE OPTIONS ANY", s, method)
	}
	if !strings.HasPrefix(path, "/") {
		return Pattern{}, fmt.Errorf("match %q: path must start with \"/\"", s)
	}
	if strings.ContainsAny(path, "?#") {
		return Pattern{}, fmt.Errorf("match %q: the query string is not part of a match", s)
	}
	p := Pattern{Method: method, raw: method + " " + path}
	if path == "/" {
		return p, nil
	}
	parts := strings.Split(path[1:], "/")
	for i, part := range parts {
		switch {
		case part == "**":
			if i != len(parts)-1 {
				return Pattern{}, fmt.Errorf("match %q: ** may only be the last segment", s)
			}
			p.segs = append(p.segs, segment{kind: segRest})
		case paramRE.MatchString(part):
			p.segs = append(p.segs, segment{kind: segParam})
		case part == "." || part == "..":
			return Pattern{}, fmt.Errorf("match %q: dot segments are not allowed", s)
		case literalRE.MatchString(part):
			p.segs = append(p.segs, segment{kind: segLiteral, lit: part})
		default:
			return Pattern{}, fmt.Errorf("match %q: segment %q must be a literal of [A-Za-z0-9._~-], {name} or **", s, part)
		}
	}
	return p, nil
}

// IsCatchAll reports whether the pattern uses ANY or ends in `**`.
func (p Pattern) IsCatchAll() bool {
	return p.Method == MethodAny || (len(p.segs) > 0 && p.segs[len(p.segs)-1].kind == segRest)
}

// matches reports whether the pattern admits the request's method and
// segments. segs is the output of CleanPath.
func (p Pattern) matches(method string, segs []string) bool {
	if p.Method != MethodAny && p.Method != method {
		return false
	}
	for i, ps := range p.segs {
		if ps.kind == segRest {
			return true
		}
		if i >= len(segs) {
			return false
		}
		switch ps.kind {
		case segLiteral:
			if segs[i] != ps.lit {
				return false
			}
		case segParam:
			if segs[i] == "" {
				return false
			}
		}
	}
	return len(segs) == len(p.segs)
}

// shapeKey identifies a pattern's shape for duplicate detection; parameter
// names do not matter.
func (p Pattern) shapeKey() string {
	var b strings.Builder
	b.WriteString(p.Method)
	for _, s := range p.segs {
		b.WriteByte('/')
		switch s.kind {
		case segLiteral:
			b.WriteString("L:" + s.lit)
		case segParam:
			b.WriteString("P")
		case segRest:
			b.WriteString("R")
		}
	}
	return b.String()
}

// moreSpecific reports whether a beats b: compared segment by segment from
// the left, a literal beats `{name}` beats `**` beats no segment; if the
// paths tie, a concrete method beats ANY.
func (a Pattern) moreSpecific(b Pattern) bool {
	rank := func(p Pattern, i int) int {
		if i >= len(p.segs) {
			return 0
		}
		switch p.segs[i].kind {
		case segLiteral:
			return 3
		case segParam:
			return 2
		}
		return 1
	}
	n := len(a.segs)
	if len(b.segs) > n {
		n = len(b.segs)
	}
	for i := 0; i < n; i++ {
		if ra, rb := rank(a, i), rank(b, i); ra != rb {
			return ra > rb
		}
	}
	return a.Method != MethodAny && b.Method == MethodAny
}
