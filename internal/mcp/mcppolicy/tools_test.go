package mcppolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func mustHash(t *testing.T, raw string) string {
	t.Helper()
	h, err := ToolHash([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestToolHashIgnoresOrderAndUnrelatedFields(t *testing.T) {
	a := mustHash(t, `{"name":"t","description":"d","inputSchema":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}}}}`)
	b := mustHash(t, `{ "inputSchema": {"properties":{"b":{"type":"number"},"a":{"type":"string"}},"type":"object"}, "annotations":{"readOnlyHint":true}, "description":"d", "_meta":{"x":1}, "name":"t" }`)
	if a != b {
		t.Error("key order and unrelated fields must not change the hash")
	}
	if len(a) != 64 {
		t.Errorf("want sha256 hex, got %q", a)
	}
}

func TestToolHashChangesWithRelevantFields(t *testing.T) {
	base := `{"name":"t","title":"T","description":"d","inputSchema":{"type":"object"},"outputSchema":{"type":"object"}}`
	h := mustHash(t, base)
	variants := []string{
		`{"name":"u","title":"T","description":"d","inputSchema":{"type":"object"},"outputSchema":{"type":"object"}}`,
		`{"name":"t","title":"U","description":"d","inputSchema":{"type":"object"},"outputSchema":{"type":"object"}}`,
		`{"name":"t","title":"T","description":"d2","inputSchema":{"type":"object"},"outputSchema":{"type":"object"}}`,
		`{"name":"t","title":"T","description":"d","inputSchema":{"type":"array"},"outputSchema":{"type":"object"}}`,
		`{"name":"t","title":"T","description":"d","inputSchema":{"type":"object"},"outputSchema":{"type":"array"}}`,
		`{"name":"t","title":"T","description":"d","inputSchema":{"type":"object"}}`,
	}
	for _, v := range variants {
		if mustHash(t, v) == h {
			t.Errorf("hash unchanged for %s", v)
		}
	}
	if mustHash(t, `{"name":"t"}`) == mustHash(t, `{"name":"t","description":""}`) {
		t.Error("an empty description differs from a missing one")
	}
}

func TestToolHashKeepsNumberPrecision(t *testing.T) {
	a := mustHash(t, `{"name":"t","inputSchema":{"maximum":9007199254740993}}`)
	b := mustHash(t, `{"name":"t","inputSchema":{"maximum":9007199254740992}}`)
	if a == b {
		t.Error("large numbers must not collapse through float64")
	}
}

func TestToolHashErrors(t *testing.T) {
	for _, raw := range []string{`[]`, `{}`, `{"name":""}`, `{"name":5}`, `{"name":null}`, `nope`} {
		if _, err := ToolHash([]byte(raw)); err == nil {
			t.Errorf("%s: want error", raw)
		}
	}
}

func TestListHash(t *testing.T) {
	a := ListHash(map[string]string{"a": "1", "b": "2"})
	b := ListHash(map[string]string{"b": "2", "a": "1"})
	if a != b || len(a) != 64 {
		t.Error("ListHash must not depend on map order")
	}
	for _, other := range []map[string]string{{"a": "1"}, {"a": "1", "b": "3"}, {"a": "1", "c": "2"}, {}} {
		if ListHash(other) == a {
			t.Errorf("%v hashes like the original", other)
		}
	}
	if ListHash(map[string]string{"a": "1\nb:2"}) == a {
		t.Error("a hash containing a newline must not forge another list")
	}
}

func TestCompare(t *testing.T) {
	review := &Review{Tools: map[string]string{"same": "h1", "changed": "h2", "gone": "h3", "gone2": "h4"}}
	live := []ToolInfo{{Name: "zeta", Hash: "x"}, {Name: "same", Hash: "h1"}, {Name: "changed", Hash: "new"}, {Name: "alpha", Hash: "y"}}
	got := Compare(review, live)
	want := Changes{Added: []string{"alpha", "zeta"}, Removed: []string{"gone", "gone2"}, Changed: []string{"changed"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got.Empty() {
		t.Error("changes is not empty")
	}
	if d := Compare(review, []ToolInfo{{Name: "same", Hash: "h1"}, {Name: "changed", Hash: "h2"}, {Name: "gone", Hash: "h3"}, {Name: "gone2", Hash: "h4"}}); !d.Empty() {
		t.Errorf("matching list changed: %+v", d)
	}
}

func TestCompareNilPin(t *testing.T) {
	got := Compare(nil, []ToolInfo{{Name: "b", Hash: "1"}, {Name: "a", Hash: "2"}})
	if !reflect.DeepEqual(got, Changes{Added: []string{"a", "b"}}) {
		t.Errorf("got %+v", got)
	}
	if !Compare(nil, nil).Empty() {
		t.Error("nothing live and no review is no changes")
	}
}

func TestCompareDuplicateLiveNames(t *testing.T) {
	review := &Review{Tools: map[string]string{"a": "1"}}
	d := Compare(review, []ToolInfo{{Name: "a", Hash: "1"}, {Name: "a", Hash: "2"}})
	if !reflect.DeepEqual(d.Changed, []string{"a"}) {
		t.Errorf("duplicate with different content must count as changed: %+v", d)
	}
}

func TestPropose(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		info ToolInfo
		want Class
	}{
		{"read only", ToolInfo{ReadOnlyHint: &yes}, Read},
		{"read only wins over destructive", ToolInfo{ReadOnlyHint: &yes, DestructiveHint: &yes}, Read},
		{"not destructive", ToolInfo{DestructiveHint: &no}, Write},
		{"read only false and not destructive", ToolInfo{ReadOnlyHint: &no, DestructiveHint: &no}, Write},
		{"destructive", ToolInfo{DestructiveHint: &yes}, Delete},
		{"no hints", ToolInfo{}, Delete},
		{"read only false only", ToolInfo{ReadOnlyHint: &no}, Delete},
	}
	for _, tc := range cases {
		got, reason := Propose(tc.info)
		if got != tc.want || reason == "" {
			t.Errorf("%s: %q %q", tc.name, got, reason)
		}
	}
}

type fakeRT struct {
	pages  map[string]string // cursor -> result JSON
	calls  []any
	err    error
	method string
}

func (f *fakeRT) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	f.method = method
	f.calls = append(f.calls, params)
	if f.err != nil {
		return nil, f.err
	}
	cursor := ""
	if m, ok := params.(map[string]any); ok {
		cursor, _ = m["cursor"].(string)
	}
	res, ok := f.pages[cursor]
	if !ok {
		return nil, fmt.Errorf("no page for cursor %q", cursor)
	}
	return json.RawMessage(res), nil
}

func TestListToolsFollowsCursor(t *testing.T) {
	rt := &fakeRT{pages: map[string]string{
		"":   `{"tools":[{"name":"a","annotations":{"readOnlyHint":true}},{"name":"b","annotations":{"destructiveHint":false}}],"nextCursor":"p2"}`,
		"p2": `{"tools":[{"name":"c"}]}`,
	}}
	tools, err := ListTools(context.Background(), rt)
	if err != nil {
		t.Fatal(err)
	}
	if rt.method != "tools/list" || len(rt.calls) != 2 {
		t.Fatalf("method %q, calls %v", rt.method, rt.calls)
	}
	if len(tools) != 3 || tools[0].Name != "a" || tools[2].Name != "c" {
		t.Fatalf("tools = %+v", tools)
	}
	if tools[0].ReadOnlyHint == nil || !*tools[0].ReadOnlyHint || tools[0].DestructiveHint != nil {
		t.Errorf("a hints: %+v", tools[0])
	}
	if tools[1].DestructiveHint == nil || *tools[1].DestructiveHint || tools[1].ReadOnlyHint != nil {
		t.Errorf("b hints: %+v", tools[1])
	}
	if tools[2].ReadOnlyHint != nil || tools[2].DestructiveHint != nil {
		t.Errorf("c hints: %+v", tools[2])
	}
	for _, tl := range tools {
		if want := mustHash(t, string(tl.Raw)); tl.Hash != want || len(tl.Raw) == 0 {
			t.Errorf("%s: hash %q raw %s", tl.Name, tl.Hash, tl.Raw)
		}
	}
}

func TestListToolsIgnoresWrongTypedHints(t *testing.T) {
	rt := &fakeRT{pages: map[string]string{"": `{"tools":[{"name":"a","annotations":{"readOnlyHint":"yes","destructiveHint":false}}]}`}}
	tools, err := ListTools(context.Background(), rt)
	if err != nil || len(tools) != 1 {
		t.Fatal(err, tools)
	}
	if tools[0].ReadOnlyHint != nil {
		t.Error("a hint of the wrong type must be ignored")
	}
}

func TestListToolsEmpty(t *testing.T) {
	rt := &fakeRT{pages: map[string]string{"": `{"tools":[]}`}}
	tools, err := ListTools(context.Background(), rt)
	if err != nil || len(tools) != 0 {
		t.Errorf("%v %v", err, tools)
	}
}

func TestListToolsFailures(t *testing.T) {
	t.Run("transport error", func(t *testing.T) {
		boom := errors.New("boom")
		if _, err := ListTools(context.Background(), &fakeRT{err: boom}); !errors.Is(err, boom) {
			t.Errorf("got %v", err)
		}
	})
	t.Run("bad result", func(t *testing.T) {
		if _, err := ListTools(context.Background(), &fakeRT{pages: map[string]string{"": `[]`}}); err == nil {
			t.Error("want error")
		}
	})
	t.Run("tool without name", func(t *testing.T) {
		if _, err := ListTools(context.Background(), &fakeRT{pages: map[string]string{"": `{"tools":[{"description":"x"}]}`}}); err == nil {
			t.Error("want error")
		}
	})
	t.Run("duplicate name across pages", func(t *testing.T) {
		rt := &fakeRT{pages: map[string]string{"": `{"tools":[{"name":"a"}],"nextCursor":"2"}`, "2": `{"tools":[{"name":"a"}]}`}}
		if _, err := ListTools(context.Background(), rt); err == nil || !strings.Contains(err.Error(), "twice") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("same cursor forever", func(t *testing.T) {
		rt := &fakeRT{pages: map[string]string{"": `{"tools":[],"nextCursor":"x"}`, "x": `{"tools":[],"nextCursor":"x"}`}}
		if _, err := ListTools(context.Background(), rt); err == nil {
			t.Error("want error")
		}
	})
}

type endlessRT struct{ pages int }

func (e *endlessRT) Call(_ context.Context, _ string, _ any) (json.RawMessage, error) {
	e.pages++
	return json.RawMessage(fmt.Sprintf(`{"tools":[{"name":"t%d"}],"nextCursor":"c%d"}`, e.pages, e.pages)), nil
}

func TestListToolsPageCap(t *testing.T) {
	rt := &endlessRT{}
	if _, err := ListTools(context.Background(), rt); err == nil {
		t.Fatal("want error")
	}
	if rt.pages != 100 {
		t.Errorf("read %d pages, want 100", rt.pages)
	}
}

func TestListToolsToolCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"tools":[`)
	for i := 0; i < 10001; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"t%d"}`, i)
	}
	b.WriteString(`]}`)
	if _, err := ListTools(context.Background(), &fakeRT{pages: map[string]string{"": b.String()}}); err == nil {
		t.Error("want error above 10,000 tools")
	}
	b.Reset()
	b.WriteString(`{"tools":[`)
	for i := 0; i < 10000; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"t%d"}`, i)
	}
	b.WriteString(`]}`)
	tools, err := ListTools(context.Background(), &fakeRT{pages: map[string]string{"": b.String()}})
	if err != nil || len(tools) != 10000 {
		t.Errorf("exactly 10,000 tools must pass: %v %d", err, len(tools))
	}
}

func TestListToolsReadsTheExactNameKey(t *testing.T) {
	rt := &fakeRT{pages: map[string]string{"": `{"tools":[{"name":"get","NAME":"rm","description":"reads"}]}`}}
	tools, err := ListTools(context.Background(), rt)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "get" {
		t.Fatalf("tools = %+v; the client sees the tool as get", tools)
	}
}
