package cli

import (
	"flag"
	"io"
	"reflect"
	"testing"
)

// testFlagSet mirrors the shape real subcommands declare: a couple of
// value-taking flags and a couple of booleans. reorderFlagsFirst needs the
// distinction to know which flags may absorb the following token.
func testFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("from", "", "")
	fs.String("harness", "", "")
	fs.Bool("force", false, "")
	fs.Bool("global", false, "")
	return fs
}

func TestReorderFlagsFirst(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "flags after positional are moved first",
			in:   []string{"echo-rest", "--from", "/tmp/f"},
			want: []string{"--from", "/tmp/f", "echo-rest"},
		},
		{
			name: "flags already first are preserved",
			in:   []string{"--from", "/tmp/f", "echo-rest"},
			want: []string{"--from", "/tmp/f", "echo-rest"},
		},
		{
			name: "= form is kept adjacent",
			in:   []string{"echo-rest", "--from=/tmp/f"},
			want: []string{"--from=/tmp/f", "echo-rest"},
		},
		{
			name: "double dash stops reordering",
			in:   []string{"echo-rest", "--", "--not-a-flag", "x"},
			want: []string{"echo-rest", "--not-a-flag", "x"},
		},
		{
			name: "bare dash is a positional",
			in:   []string{"-", "--from", "/tmp/f"},
			want: []string{"--from", "/tmp/f", "-"},
		},
		{
			name: "bool flag (no value) is reordered alone",
			in:   []string{"echo-rest", "--force"},
			want: []string{"--force", "echo-rest"},
		},
		{
			name: "bool flag does not absorb the following positional",
			in:   []string{"--force", "echo-rest"},
			want: []string{"--force", "echo-rest"},
		},
		{
			name: "unknown flag keeps the value-taking behavior",
			in:   []string{"--bogus", "value", "echo-rest"},
			want: []string{"--bogus", "value", "echo-rest"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reorderFlagsFirst(testFlagSet(), c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("reorderFlagsFirst(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestReorderFlagsFirst_BoolFlagDoesNotSwallowPositional pins the fix for
// issue #227. A boolean flag written before the positional used to absorb the
// skill name ("--force echo-rest"), which made flag.Parse stop at that
// positional and silently discard every flag after it — so
// `omac register --force <skill> --harness opencode` died with a usage dump
// while the same flags in a different order worked.
func TestReorderFlagsFirst_BoolFlagDoesNotSwallowPositional(t *testing.T) {
	orders := [][]string{
		{"--force", "echo-rest", "--harness", "opencode"},
		{"echo-rest", "--force", "--harness", "opencode"},
		{"--force", "--harness", "opencode", "echo-rest"},
	}
	for _, args := range orders {
		fs := testFlagSet()
		if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
			t.Fatalf("args %v: parse: %v", args, err)
		}
		if got := fs.NArg(); got != 1 {
			t.Fatalf("args %v: NArg = %d (%v), want 1", args, got, fs.Args())
		}
		if got := fs.Arg(0); got != "echo-rest" {
			t.Errorf("args %v: positional = %q, want echo-rest", args, got)
		}
		if got := fs.Lookup("force").Value.String(); got != "true" {
			t.Errorf("args %v: --force = %s, want true", args, got)
		}
		if got := fs.Lookup("harness").Value.String(); got != "opencode" {
			t.Errorf("args %v: --harness = %q, want opencode", args, got)
		}
	}
}
