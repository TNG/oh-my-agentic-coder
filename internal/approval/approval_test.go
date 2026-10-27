package approval

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func mustSet(t *testing.T, entries ...Entry) *Set {
	t.Helper()
	s, err := Compile(entries)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return s
}

func lookup(t *testing.T, s *Set, method, raw string) (Entry, bool) {
	t.Helper()
	segs, _, err := CleanPath(raw)
	if err != nil {
		t.Fatalf("CleanPath(%q): %v", raw, err)
	}
	return s.Lookup(method, segs)
}

func TestParsePatternRejects(t *testing.T) {
	for _, in := range []string{
		"", "GET", "GET /a b", "get /a", "FETCH /a", "GET a", "GET /a?x=1", "GET /a#f",
		"GET /**/a", "GET /a/..", "GET /a/./b", "GET /{}", "GET /{1x}", "GET /a*", "GET /a%2fb", "GET /re(gex)",
	} {
		if _, err := ParsePattern(in); err == nil {
			t.Errorf("ParsePattern(%q) accepted", in)
		}
	}
}

func TestCompileRejects(t *testing.T) {
	cases := map[string][]Entry{
		"empty":            nil,
		"bad mode":         {{Match: "GET /a", Approval: "maybe"}},
		"none on ANY":      {{Match: "ANY /a", Approval: None}},
		"none on **":       {{Match: "GET /a/**", Approval: None}},
		"duplicate":        {{Match: "GET /a", Approval: None}, {Match: "GET /a", Approval: Required}},
		"param name alias": {{Match: "GET /a/{x}", Approval: None}, {Match: "GET /a/{y}", Approval: Required}},
	}
	for name, es := range cases {
		if _, err := Compile(es); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMostSpecificWins(t *testing.T) {
	s := mustSet(t,
		Entry{Match: "GET /live/{id}", Approval: None},
		Entry{Match: "GET /live/start", Approval: Required},
		Entry{Match: "ANY /**", Approval: Required},
		Entry{Match: "POST /live/{id}", Approval: Required},
	)
	for _, c := range []struct {
		method, path string
		want         Mode
		ok           bool
	}{
		{"GET", "/live/start", Required, true},
		{"GET", "/live/abc", None, true},
		{"POST", "/live/abc", Required, true},
		{"DELETE", "/live/abc", Required, true}, // falls to ANY /**
		{"GET", "/other/x/y", Required, true},
		{"GET", "/", Required, true}, // ** matches zero segments
	} {
		e, ok := lookup(t, s, c.method, c.path)
		if ok != c.ok || e.Approval != c.want {
			t.Errorf("%s %s = %v,%v want %v,%v", c.method, c.path, e.Approval, ok, c.want, c.ok)
		}
	}
}

func TestClosedWorld(t *testing.T) {
	s := mustSet(t,
		Entry{Match: "GET /status", Approval: None},
		Entry{Match: "POST /echo", Approval: Required},
	)
	for _, c := range []struct{ method, path string }{
		{"GET", "/new-route"}, {"POST", "/status"}, {"GET", "/status/"}, {"GET", "/status/x"},
		{"HEAD", "/status"}, {"GET", "/"},
	} {
		if e, ok := lookup(t, s, c.method, c.path); ok {
			t.Errorf("%s %s matched %q", c.method, c.path, e.Match)
		}
	}
}

func TestPathParamNeedsNonEmptySegment(t *testing.T) {
	s := mustSet(t, Entry{Match: "GET /live/{id}", Approval: None})
	if _, ok := lookup(t, s, "GET", "/live/"); ok {
		t.Error("{id} matched an empty segment")
	}
}

func TestCleanPathBypasses(t *testing.T) {
	s := mustSet(t,
		Entry{Match: "GET /public/{x}", Approval: None},
		Entry{Match: "ANY /**", Approval: Required},
	)
	for _, raw := range []string{
		"/public/../admin/x", "//admin/x", "/public/%2e%2e/admin/x", "/public/%2E%2E/admin/x",
		"/./admin/x", "/public/./../admin/x", "/../../admin/x",
	} {
		segs, fwd, err := CleanPath(raw)
		if err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if fwd != "/admin/x" {
			t.Errorf("%q forwarded as %q", raw, fwd)
		}
		if e, _ := s.Lookup("GET", segs); e.Approval != Required {
			t.Errorf("%q matched %s", raw, e.Match)
		}
	}
}

func TestCleanPathRejectsAmbiguous(t *testing.T) {
	for _, raw := range []string{
		"/a%2fb", "/a%2Fb", "/a%5cb", "/a\\b", "/a%00b", "/a%0ab", "/a%zz", "/a%",
	} {
		if _, _, err := CleanPath(raw); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestCleanPathForwardMatchesMatched(t *testing.T) {
	cases := map[string]struct {
		segs []string
		fwd  string
	}{
		"":              {nil, "/"},
		"/":             {nil, "/"},
		"/a/b":          {[]string{"a", "b"}, "/a/b"},
		"/a/b/":         {[]string{"a", "b", ""}, "/a/b/"},
		"/a/..":         {nil, "/"},
		"/a b":          {[]string{"a b"}, "/a%20b"},
		"/a%20b":        {[]string{"a b"}, "/a%20b"},
		"/%252e%252e/x": {[]string{"%2e%2e", "x"}, "/%252e%252e/x"},
		"/a/b/../c":     {[]string{"a", "c"}, "/a/c"},
		"/a//b":         {[]string{"a", "b"}, "/a/b"},
		"/caf%C3%A9/":   {[]string{"café", ""}, "/caf%C3%A9/"},
		"/a;x=1/b":      {[]string{"a;x=1", "b"}, "/a;x=1/b"},
	}
	for raw, want := range cases {
		segs, fwd, err := CleanPath(raw)
		if err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if !reflect.DeepEqual(segs, want.segs) && !(len(segs) == 0 && len(want.segs) == 0) {
			t.Errorf("%q segs = %q want %q", raw, segs, want.segs)
		}
		if fwd != want.fwd {
			t.Errorf("%q fwd = %q want %q", raw, fwd, want.fwd)
		}
		// Round trip: cleaning the forwarded path changes nothing.
		segs2, fwd2, err := CleanPath(fwd)
		if err != nil || fwd2 != fwd || !(reflect.DeepEqual(segs2, segs) || (len(segs) == 0 && len(segs2) == 0)) {
			t.Errorf("%q: forwarded %q is not stable (%q, %v)", raw, fwd, fwd2, err)
		}
	}
}

func TestStripOverrideHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("x-http-method-override", "DELETE")
	h.Set("X-Method-Override", "PUT")
	h.Set("X-Other", "keep")
	got := StripOverrideHeaders(h)
	if len(got) != 2 || h.Get("X-HTTP-Method-Override") != "" || h.Get("X-Method-Override") != "" || h.Get("X-Other") != "keep" {
		t.Errorf("removed %v, headers %v", got, h)
	}
	if !strings.Contains(strings.Join(got, ","), "X-HTTP-Method-Override") {
		t.Errorf("names = %v", got)
	}
}
