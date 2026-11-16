package vm

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestNameHints(t *testing.T) {
	cases := []struct{ in, want string }{
		{"omac vm is not created; run `omac vm up`", "omac vm is not created; run `omac vm up --name yarp3`"},
		{"run `omac vm up` again (or `omac vm up --reprovision`)", "run `omac vm up --name yarp3` again (or `omac vm up --name yarp3 --reprovision`)"},
		{"run `omac vm offline` before attaching", "run `omac vm offline --name yarp3` before attaching"},
		{"is missing; run `omac vm reset`", "is missing; run `omac vm reset --name yarp3`"},
		{"try `omac vm status` or `omac vm list`", "try `omac vm status` or `omac vm list`"},
		{"no hint", "no hint"},
	}
	for _, c := range cases {
		if got := NameHints("yarp3", c.in); got != c.want {
			t.Errorf("NameHints(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := NameHints(DefaultVMName, c.in); got != c.in {
			t.Errorf("NameHints for the default VM changed %q to %q", c.in, got)
		}
	}
}

func TestWithNameHintsKeepsErrorIdentity(t *testing.T) {
	err := WithNameHints("yarp3", fmt.Errorf("omac vm: %w", ErrNotCreated))
	if !errors.Is(err, ErrNotCreated) {
		t.Error("errors.Is lost the sentinel")
	}
	want := "omac vm: omac vm is not created; run `omac vm up --name yarp3`"
	if err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	if WithNameHints("yarp3", nil) != nil {
		t.Error("nil error must stay nil")
	}
}

func TestNameHintWriter(t *testing.T) {
	var buf bytes.Buffer
	w := NameHintWriter("yarp3", &buf)
	n, err := fmt.Fprintln(w, "omac vm: run `omac vm offline` before attaching")
	if err != nil || n != len("omac vm: run `omac vm offline` before attaching\n") {
		t.Errorf("Fprintln = (%d, %v), want the caller's byte count", n, err)
	}
	if got, want := buf.String(), "omac vm: run `omac vm offline --name yarp3` before attaching\n"; got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
}
