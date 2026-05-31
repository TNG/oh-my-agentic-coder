package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
)

// canonicalizeFieldValue is the type-checking layer between user input
// (prompt, --fields-from file, OMAC_CONFIG_<NAME> env var) and the
// JSON store. The behavioral contract is:
//
//   string  -- regex-validated if Pattern set; preserved verbatim.
//   bool    -- accepts the human spellings; canonical form is "true"/"false".
//   int     -- accepts base-10; canonical form is the strconv rendering.
//   enum    -- exact match against Choices; preserved verbatim on hit.
//
// These tests pin the contract so prompt + non-interactive paths stay
// in sync.

func TestCanonicalize_String(t *testing.T) {
	spec := config.ConfigSpec{Name: "F", Type: config.ConfigFieldString}
	got, err := canonicalizeFieldValue(spec, "hello world")
	if err != nil || got != "hello world" {
		t.Errorf("string: got %q,%v; want %q,nil", got, err, "hello world")
	}
}

func TestCanonicalize_StringPattern(t *testing.T) {
	spec := config.ConfigSpec{Name: "F", Type: config.ConfigFieldString, Pattern: `^[a-z]+$`}
	if _, err := canonicalizeFieldValue(spec, "hello"); err != nil {
		t.Errorf("matching: unexpected err %v", err)
	}
	if _, err := canonicalizeFieldValue(spec, "Hello"); err == nil {
		t.Error("non-matching: expected error")
	}
}

func TestCanonicalize_Bool(t *testing.T) {
	spec := config.ConfigSpec{Name: "F", Type: config.ConfigFieldBool}
	for _, in := range []string{"yes", "Y", "1", "TRUE", "on"} {
		got, err := canonicalizeFieldValue(spec, in)
		if err != nil || got != "true" {
			t.Errorf("bool(%q) = %q,%v; want \"true\",nil", in, got, err)
		}
	}
	for _, in := range []string{"no", "N", "0", "FALSE", "off"} {
		got, err := canonicalizeFieldValue(spec, in)
		if err != nil || got != "false" {
			t.Errorf("bool(%q) = %q,%v; want \"false\",nil", in, got, err)
		}
	}
	if _, err := canonicalizeFieldValue(spec, "maybe"); err == nil {
		t.Error("bool(maybe): expected error")
	}
}

func TestCanonicalize_Int(t *testing.T) {
	spec := config.ConfigSpec{Name: "F", Type: config.ConfigFieldInt}
	for _, c := range []struct{ in, want string }{
		{"42", "42"},
		{"-7", "-7"},
		{"  100 ", "100"}, // whitespace tolerated
		{"0", "0"},
	} {
		got, err := canonicalizeFieldValue(spec, c.in)
		if err != nil || got != c.want {
			t.Errorf("int(%q) = %q,%v; want %q,nil", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"twelve", "1.5", "0x10", ""} {
		if _, err := canonicalizeFieldValue(spec, in); err == nil {
			t.Errorf("int(%q): expected error", in)
		}
	}
}

func TestCanonicalize_Enum(t *testing.T) {
	spec := config.ConfigSpec{
		Name: "F", Type: config.ConfigFieldEnum,
		Choices: []string{"alpha", "bravo", "charlie"},
	}
	if got, err := canonicalizeFieldValue(spec, "bravo"); err != nil || got != "bravo" {
		t.Errorf("enum hit: got %q,%v", got, err)
	}
	_, err := canonicalizeFieldValue(spec, "delta")
	if err == nil {
		t.Fatal("enum miss: expected error")
	}
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "charlie") {
		t.Errorf("enum miss error %q should list every choice", err)
	}
}

func TestCanonicalize_DefaultStringType(t *testing.T) {
	// Empty .Type should behave as string.
	spec := config.ConfigSpec{Name: "F"}
	got, err := canonicalizeFieldValue(spec, "anything goes")
	if err != nil || got != "anything goes" {
		t.Errorf("default-type: got %q,%v", got, err)
	}
}

// envWithStdin returns an *Env whose Stdin reads from input and whose
// stdout/stderr go to /dev/null. Used by the handleOneField tests
// below; the prompt path of handleOneField goes through a real
// *os.File so we wire one up via os.Pipe.
func envWithStdin(t *testing.T, input string) *Env {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	go func() {
		defer w.Close()
		_, _ = io.WriteString(w, input)
	}()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() {
		r.Close()
		null.Close()
	})
	return &Env{Stdin: r, Stdout: null, Stderr: null, Version: "test"}
}

// TestHandleOneField_PreviouslySkippedOptional pins the fix for the
// bug "register --force re-asks every optional field". An optional
// field that the user previously declined (recorded in the registry's
// SkippedConfigFields list) MUST NOT be prompted for again on a
// subsequent register, regardless of --force.
//
// We can prove "no prompt happened" by feeding stdin with content
// that, if read, would store a value: handleOneField returns the
// skipped flag and leaves the store untouched.
func TestHandleOneField_PreviouslySkippedOptional(t *testing.T) {
	env := envWithStdin(t, "hello\n") // would-be input if prompted
	store := &skillconfig.Store{}
	// required: false  +  prevSkipped[F] = true  =>  must skip silently.
	r := false
	spec := config.ConfigSpec{Name: "F", Type: config.ConfigFieldString, Required: &r}

	skipped, err := handleOneField(env, store, "skill", spec, false, map[string]bool{"F": true}, nil, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !skipped {
		t.Errorf("skipped = false, want true (optional + previously declined)")
	}
	if _, ok := store.Get("skill", "F"); ok {
		t.Errorf("store should not have been written; got value present")
	}
}

// TestHandleOneField_PreviouslySkippedRequired ensures the skip
// memory does NOT short-circuit a required field. If a previously
// optional field is upgraded to required (or the registry was hand-
// edited), we must re-prompt and let the normal "refused after 3
// attempts" path run.
func TestHandleOneField_PreviouslySkippedRequired(t *testing.T) {
	// 3 empty lines simulate the user mashing Enter; a required field
	// then refuses with "required config field not supplied".
	env := envWithStdin(t, "\n\n\n")
	store := &skillconfig.Store{}
	spec := config.ConfigSpec{Name: "F", Type: config.ConfigFieldString} // required by default

	_, err := handleOneField(env, store, "skill", spec, false, map[string]bool{"F": true}, nil, false, nil)
	if err == nil || !strings.Contains(err.Error(), "refused: required config field") {
		t.Errorf("expected refused-required error, got %v", err)
	}
}

// TestHandleOneField_RepromptIgnoresPrevSkipped verifies that
// --reprompt-fields (reprompt=true) bypasses the previously-declined
// memory: the user is asked again, and a fresh value is stored.
func TestHandleOneField_RepromptIgnoresPrevSkipped(t *testing.T) {
	env := envWithStdin(t, "fresh-value\n")
	store := &skillconfig.Store{}
	r := false
	spec := config.ConfigSpec{Name: "F", Type: config.ConfigFieldString, Required: &r}

	skipped, err := handleOneField(env, store, "skill", spec, true, map[string]bool{"F": true}, nil, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skipped {
		t.Errorf("skipped = true; reprompt=true must override prev-skipped memory")
	}
	if v, ok := store.Get("skill", "F"); !ok || v != "fresh-value" {
		t.Errorf("store[F] = %q,%v; want fresh-value,true", v, ok)
	}
}

// TestHandleOneField_OptionalSkipReturnsTrue checks the freshly-
// skipped path: nothing in prev, user enters empty, optional field.
// The bool return must be true so the caller records this in the
// registry for next time.
func TestHandleOneField_OptionalSkipReturnsTrue(t *testing.T) {
	env := envWithStdin(t, "\n")
	store := &skillconfig.Store{}
	r := false
	spec := config.ConfigSpec{Name: "F", Type: config.ConfigFieldString, Required: &r}

	skipped, err := handleOneField(env, store, "skill", spec, false, nil, nil, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !skipped {
		t.Errorf("skipped = false; freshly-skipped optional must return true")
	}
	if _, ok := store.Get("skill", "F"); ok {
		t.Errorf("optional skip must not write to store")
	}
}

// TestHandleOneField_AlreadyConfiguredReturnsFalse verifies that the
// "already in store" short-circuit returns skipped=false. It's a
// real value, not a skip — we don't want it appearing in the
// registry's SkippedConfigFields list.
func TestHandleOneField_AlreadyConfiguredReturnsFalse(t *testing.T) {
	env := envWithStdin(t, "")
	store := &skillconfig.Store{}
	store.Set("skill", "F", "previously-stored")
	spec := config.ConfigSpec{Name: "F", Type: config.ConfigFieldString}

	skipped, err := handleOneField(env, store, "skill", spec, false, map[string]bool{"F": true}, nil, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if skipped {
		t.Errorf("already-configured fields must not count as skipped")
	}
}

// TestDedupSorted exercises the helper that prepares the registry's
// skip list for serialization: stable order, no duplicates, nil for
// empty input so the JSON omitempty tag elides the field on disk.
func TestDedupSorted(t *testing.T) {
	if got := dedupSorted(nil); got != nil {
		t.Errorf("nil input: got %v, want nil", got)
	}
	if got := dedupSorted([]string{}); got != nil {
		t.Errorf("empty input: got %v, want nil", got)
	}
	got := dedupSorted([]string{"c", "a", "b", "a", "c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("dedupSorted: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dedupSorted[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
