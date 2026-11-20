package netprompt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"unicode"
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
// which the sandboxed agent does not control. "0", "false", "no" and "off"
// turn it off, as for OMAC_PROMPT_STUB.
func NewChooser() (*Chooser, bool) {
	c := &Chooser{stub: approvalStub()}
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

func approvalStub() string {
	v := strings.TrimSpace(os.Getenv("OMAC_APPROVAL_STUB"))
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return ""
	}
	return v
}

// Stub returns the label every dialog is answered with without being shown,
// or "" when dialogs are real.
func (c *Chooser) Stub() string { return c.stub }

// Choose blocks until the user picks a label, cancels (returns "", nil) or
// ctx ends (returns ctx.Err()). The default label is preselected. text is
// sanitized here; title and labels are chosen by the caller and not user data.
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

// Notify shows a passive notification that a decision is waiting. It
// returns at once: a notifier that hangs must not hold up the dialog.
func (c *Chooser) Notify(title, body string) {
	if c.stub == "" {
		go osNotify(title, body)
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
		// Line and paragraph separators render as line breaks, and format
		// characters such as U+202E reorder text, so the agent could forge a
		// line of the dialog through the query or path it controls.
		if unicode.In(r, unicode.Zl, unicode.Zp, unicode.Cf) {
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
	return err == nil && hasDisplay()
}

// hasDisplay reports whether a Linux dialog can be shown at all. Without a
// display, zenity and kdialog exit 1, the same code as Cancel, so a failed
// dialog would be read as the user's "deny" and nobody would learn that no
// dialog was ever shown.
func hasDisplay() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

func (zenityChoice) choose(ctx context.Context, title, text string, labels []string, def string) (string, error) {
	cmd := exec.CommandContext(ctx, "zenity", zenityChoiceArgs(title, text, labels, def)...)
	cmd.Env = append(os.Environ(), "GTK_OVERLAY_SCROLLING=0")
	out, err := cmd.Output()
	if err != nil {
		return "", dialogExitError(ctx, "zenity", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// zenityChoiceArgs builds the radiolist argv. zenity 4 parses the text of a
// list as Pango markup even with --no-markup, and shows its own "Select
// items from the list below." instead when that fails, so a query with a
// "&" hid the whole request. The text is therefore always escaped and
// parsed, which renders the same on zenity 3 and 4. The full dialog height
// keeps every choice, including the preselected Deny, in view.
func zenityChoiceArgs(title, text string, labels []string, def string) []string {
	w, h := dialogDimensions()
	args := []string{"--list", "--radiolist", "--title", title, "--text", escapeMarkup(text),
		"--column", "", "--column", "Decision", "--width", strconv.Itoa(w), "--height", strconv.Itoa(h)}
	for _, l := range labels {
		sel := "FALSE"
		if l == def {
			sel = "TRUE"
		}
		args = append(args, sel, l)
	}
	return args
}

// escapeMarkup escapes the characters Pango markup gives a meaning to; line
// breaks stay.
func escapeMarkup(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

type kdialogChoice struct{}

func (kdialogChoice) name() string { return "kdialog" }
func (kdialogChoice) available() bool {
	_, err := exec.LookPath("kdialog")
	return err == nil && hasDisplay()
}

func (kdialogChoice) choose(ctx context.Context, title, text string, labels []string, def string) (string, error) {
	out, err := exec.CommandContext(ctx, "kdialog", kdialogChoiceArgs(title, text, labels, def)...).Output()
	if err != nil {
		return "", dialogExitError(ctx, "kdialog", err)
	}
	idx := strings.TrimSpace(string(out))
	for i, l := range labels {
		if strconv.Itoa(i) == idx {
			return l, nil
		}
	}
	return "", nil
}

func kdialogChoiceArgs(title, text string, labels []string, def string) []string {
	w, h := dialogDimensions()
	args := []string{"--geometry", fmt.Sprintf("%dx%d", w, h), "--title", title, "--radiolist", qtPlainText(text)}
	for i, l := range labels {
		state := "off"
		if l == def {
			state = "on"
		}
		args = append(args, strconv.Itoa(i), l, state)
	}
	return args
}

// qtPlainText keeps kdialog from rendering text as rich text. Qt decides
// that from the first line alone: a tag there turns the whole text into
// HTML, where "<b>" further down would style it. The first line is omac's
// own, but in case it ever holds a "<", that one is swapped for a look-alike.
func qtPlainText(text string) string {
	first, rest, found := strings.Cut(text, "\n")
	first = strings.NewReplacer("<", "‹", ">", "›").Replace(first)
	if !found {
		return first
	}
	return first + "\n" + rest
}

// dialogExitError maps a failed zenity or kdialog run to Choose's contract.
// Exit code 1 is the user's Cancel and returns nil. Anything else is an
// error: the dialog was never shown, and reading it as Cancel would record a
// deny nobody gave. zenity 4 also exits 1 when it cannot open the display,
// so its "Failed to open display" on stderr is an error too. Other stderr
// output, such as GTK warnings about the accessibility bus, comes with a
// real Cancel as well and does not count.
func dialogExitError(ctx context.Context, name string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && !displayFailed(exit.Stderr) {
		return nil
	}
	if exit != nil && len(exit.Stderr) > 0 {
		return fmt.Errorf("%s: %w: %s", name, err, firstLine(exit.Stderr))
	}
	return fmt.Errorf("%s: %w", name, err)
}

func displayFailed(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	return strings.Contains(s, "open display") || strings.Contains(s, "connect to display")
}

func firstLine(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
