package vm

import (
	"io"
	"regexp"
	"strings"
)

// hintRE matches a quoted omac vm command that acts on one VM, as used in
// the hints of error and progress messages.
var hintRE = regexp.MustCompile("`omac vm (up|down|reset|status|online|offline|image add)((?: [^`]*)?)`")

// NameHints rewrites the omac vm commands quoted in s so they act on the
// named VM, e.g. "run `omac vm up`" becomes "run `omac vm up --name yarp3`".
// The default VM needs no flag, so s is returned unchanged; so is a command
// that already carries --name.
func NameHints(name, s string) string {
	if name == DefaultVMName {
		return s
	}
	return hintRE.ReplaceAllStringFunc(s, func(m string) string {
		if strings.Contains(m, " --name") {
			return m
		}
		sub := hintRE.FindStringSubmatch(m)
		return "`omac vm " + sub[1] + " --name " + name + sub[2] + "`"
	})
}

type hintedError struct {
	err error
	msg string
}

func (e *hintedError) Error() string { return e.msg }
func (e *hintedError) Unwrap() error { return e.err }

// WithNameHints returns err with NameHints applied to its message. errors.Is
// and errors.As still see err.
func WithNameHints(name string, err error) error {
	if err == nil || name == DefaultVMName {
		return err
	}
	return &hintedError{err: err, msg: NameHints(name, err.Error())}
}

type hintWriter struct {
	w    io.Writer
	name string
}

func (h hintWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(h.w, NameHints(h.name, string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// NameHintWriter applies NameHints to everything written to w. Each Write
// has to carry a whole message, as fmt.Fprintln does.
func NameHintWriter(name string, w io.Writer) io.Writer {
	if name == DefaultVMName || w == nil {
		return w
	}
	return hintWriter{w: w, name: name}
}
