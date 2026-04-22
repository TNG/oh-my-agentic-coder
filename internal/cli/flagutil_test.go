package cli

import (
	"reflect"
	"testing"
)

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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reorderFlagsFirst(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("reorderFlagsFirst(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
