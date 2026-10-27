package netprompt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Chooser shows one native single-choice dialog and returns the chosen label.
// It is the operation counterpart of Prompter: the sidecar approval gate asks
// "may this request go through?" with its own text and labels, not a
// host:port question.
type Chooser struct {
	backends []dialogBackendChoice
	stub     string
}

type dialogBackendChoice interface {
	name() string
	available() bool
	choose(ctx context.Context, title, text string, labels []string, def string) (string, error)
}

// NewChooser builds the platform chooser and reports whether any dialog
// backend is available. OMAC_APPROVAL_STUB, when set to a label, answers
// every dialog with that label without showing anything; it exists for
// non-interactive tests and is read from the host process's environment,
// which the sandboxed agent does not control.
func NewChooser() (*Chooser, bool) {
	c := &Chooser{stub: os.Getenv("OMAC_APPROVAL_STUB")}
	if c.stub != "" {
		return c, true
	}
	if runtime.GOOS == "darwin" {
		c.backends = []dialogBackendChoice{osascriptChoice{}}
	} else {
		c.backends = []dialogBackendChoice{zenityChoice{}, kdialogChoice{}}
	}
	for _, b := range c.backends {
		if b.available() {
			return c, true
		}
	}
	return c, false
}

// Choose blocks until the user picks a label, cancels (returns "", nil) or
// ctx ends (returns ctx.Err()). The default label is preselected. title and
// text are sanitized here; labels are chosen by the caller and not user data.
func (c *Chooser) Choose(ctx context.Context, title, text string, labels []string, def string) (string, error) {
	if c.stub != "" {
		return c.stub, nil
	}
	for _, b := range c.backends {
		if b.available() {
			return b.choose(ctx, title, sanitizeDialogText(text), labels, def)
		}
	}
	return "", fmt.Errorf("no dialog backend available")
}

// Notify shows a passive notification that a decision is waiting.
func (c *Chooser) Notify(title, body string) {
	if c.stub == "" {
		osNotify(title, body)
	}
}

// sanitizeDialogText drops control characters other than newline so request
// data cannot forge dialog structure; line breaks written by the caller stay.
func sanitizeDialogText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == '\r' || r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
}

type osascriptChoice struct{}

func (osascriptChoice) name() string { return "osascript" }
func (osascriptChoice) available() bool {
	_, err := exec.LookPath("osascript")
	return err == nil
}

func (osascriptChoice) choose(ctx context.Context, title, text string, labels []string, def string) (string, error) {
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = appleScriptString(l)
	}
	script := fmt.Sprintf(`choose from list {%s} with title %s with prompt %s default items {%s} OK button name "Select" cancel button name "Cancel"`,
		strings.Join(quoted, ", "), appleScriptString(title), appleScriptString(text), appleScriptString(def))
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	choice := strings.TrimSpace(string(out))
	if choice == "false" {
		return "", nil
	}
	return choice, nil
}

type zenityChoice struct{}

func (zenityChoice) name() string { return "zenity" }
func (zenityChoice) available() bool {
	_, err := exec.LookPath("zenity")
	return err == nil
}

func (zenityChoice) choose(ctx context.Context, title, text string, labels []string, def string) (string, error) {
	w, h := dialogDimensions()
	args := []string{"--list", "--radiolist", "--no-markup", "--title", title, "--text", text,
		"--column", "", "--column", "Decision", "--width", strconv.Itoa(w), "--height", strconv.Itoa(h / 2)}
	for _, l := range labels {
		sel := "FALSE"
		if l == def {
			sel = "TRUE"
		}
		args = append(args, sel, l)
	}
	cmd := exec.CommandContext(ctx, "zenity", args...)
	cmd.Env = append(os.Environ(), "GTK_OVERLAY_SCROLLING=0")
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

type kdialogChoice struct{}

func (kdialogChoice) name() string { return "kdialog" }
func (kdialogChoice) available() bool {
	_, err := exec.LookPath("kdialog")
	return err == nil
}

func (kdialogChoice) choose(ctx context.Context, title, text string, labels []string, def string) (string, error) {
	w, h := dialogDimensions()
	args := []string{"--geometry", fmt.Sprintf("%dx%d", w, h/2), "--title", title, "--radiolist", text}
	for i, l := range labels {
		state := "off"
		if l == def {
			state = "on"
		}
		args = append(args, strconv.Itoa(i), l, state)
	}
	out, err := exec.CommandContext(ctx, "kdialog", args...).Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", nil
	}
	idx := strings.TrimSpace(string(out))
	for i, l := range labels {
		if strconv.Itoa(i) == idx {
			return l, nil
		}
	}
	return "", nil
}
