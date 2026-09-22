// Style helpers for human-facing CLI output: ANSI colors, section
// headings, and bordered callout boxes.
//
// Coloring is automatically disabled when the destination stream is not
// a terminal, when the NO_COLOR environment variable is set (see
// https://no-color.org/), or when TERM=dumb. This keeps piped/CI output
// clean while giving interactive users a nicer experience.
package cli

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// ANSI SGR codes. Kept as small constants so the styler can assemble
// sequences without pulling in a dependency.
const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
	ansiGray   = "\033[90m"
)

// styler renders styled fragments, honoring whether color is enabled for
// a given stream. The zero value is unusable; build one with newStyler.
type styler struct {
	color bool
}

// newStyler decides whether to emit ANSI escapes for w. Color is on only
// when w is a real terminal and the environment doesn't opt out.
func newStyler(w *os.File) styler {
	return styler{color: colorEnabled(w)}
}

// colorEnabled reports whether ANSI styling should be used for w.
func colorEnabled(w *os.File) bool {
	if w == nil {
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(w.Fd()))
}

// paint wraps s in the given SGR codes when color is enabled, otherwise
// returns s untouched.
func (s styler) paint(text string, codes ...string) string {
	if !s.color || len(codes) == 0 {
		return text
	}
	return strings.Join(codes, "") + text + ansiReset
}

func (s styler) bold(text string) string  { return s.paint(text, ansiBold) }
func (s styler) dim(text string) string   { return s.paint(text, ansiDim) }
func (s styler) green(text string) string { return s.paint(text, ansiGreen) }
func (s styler) cyan(text string) string  { return s.paint(text, ansiCyan) }
func (s styler) gray(text string) string  { return s.paint(text, ansiGray) }

// heading prints a blank line then a bold/underlined-feel section title,
// e.g.
//
//	── Secrets ──────────────
func (s styler) heading(w *os.File, title string) {
	const width = 56
	label := " " + title + " "
	dashes := width - len(label) - 2
	if dashes < 2 {
		dashes = 2
	}
	line := "──" + label + strings.Repeat("─", dashes)
	fmt.Fprintf(w, "\n%s\n", s.paint(line, ansiBold, ansiCyan))
}

// status prints an indented "tag detail" status line. The tag is colored
// per its semantic kind; detail is left plain so values stay readable.
func (s styler) status(w *os.File, tag, detail string, codes ...string) {
	fmt.Fprintf(w, "  %s %s\n", s.paint(tag, codes...), detail)
}

// setLine prints a "set NAME = VALUE  (source)" assignment line with the
// name bolded, the value greened, and the optional source dimmed.
func (s styler) setLine(w *os.File, name, value, source string) {
	tail := ""
	if source != "" {
		tail = s.gray(" " + source)
	}
	fmt.Fprintf(w, "  %s %s %s %s%s\n",
		s.paint("set", ansiGreen),
		s.bold(name),
		s.gray("="),
		s.green(value),
		tail,
	)
}

// callout renders title and one or more body lines inside a rounded box.
// Used to make the post-register install-script command impossible to
// miss. Lines are not wrapped; the box grows to the widest line.
func (s styler) callout(w *os.File, accent string, title string, lines []string) {
	// Compute inner width from the visible (un-styled) content.
	inner := visibleLen(title)
	for _, l := range lines {
		if n := visibleLen(l); n > inner {
			inner = n
		}
	}
	const pad = 1
	width := inner + pad*2

	top := "╭" + strings.Repeat("─", width) + "╮"
	bot := "╰" + strings.Repeat("─", width) + "╯"
	sep := "├" + strings.Repeat("─", width) + "┤"

	border := func(text string) string { return s.paint(text, ansiBold, accent) }

	fmt.Fprintln(w)
	fmt.Fprintln(w, border(top))
	fmt.Fprintln(w, border("│")+boxPad(s.paint(title, ansiBold, accent), inner, pad)+border("│"))
	fmt.Fprintln(w, border(sep))
	for _, l := range lines {
		fmt.Fprintln(w, border("│")+boxPad(l, inner, pad)+border("│"))
	}
	fmt.Fprintln(w, border(bot))
}

// boxPad left/right-pads a (possibly styled) cell to the box interior.
// It pads based on the visible length so ANSI codes don't skew alignment.
func boxPad(cell string, inner, pad int) string {
	gap := inner - visibleLen(cell)
	if gap < 0 {
		gap = 0
	}
	return strings.Repeat(" ", pad) + cell + strings.Repeat(" ", gap) + strings.Repeat(" ", pad)
}

// wrapVisible word-wraps text to lines of at most max visible columns
// (widths measured with visibleLen, so a caller can pass the width that
// must fit inside a box). Space-separated words keep meaning; a word longer
// than max is hard-split. Text is expected plain, without ANSI escapes.
func wrapVisible(text string, max int) []string {
	if max < 1 {
		max = 1
	}
	var out []string
	var line []rune
	width := 0
	flush := func() {
		if len(line) == 0 {
			return
		}
		out = append(out, string(line))
		line = nil
		width = 0
	}
	for _, word := range strings.Fields(text) {
		runes := []rune(word)
		for len(runes) > 0 {
			w := visibleLen(string(runes))
			room := max
			if len(line) > 0 {
				room = max - width - 1 // + the joining space
			}
			switch {
			case w <= room:
				if len(line) > 0 {
					line = append(line, ' ')
					width++
				}
				line = append(line, runes...)
				width += w
				runes = nil
			default:
				// Does not fit: close the line, then emit the word in
				// full-width chunks (at most one chunk per line).
				flush()
				n := min(max, len(runes))
				line = append(line, runes[:n]...)
				width += visibleLen(string(runes[:n]))
				runes = runes[n:]
				flush()
			}
		}
	}
	flush()
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// stripControlChars removes C0/C1 control characters from s. This prevents
// terminal escape sequences planted in session ids or titles from injecting
// display commands when the text is printed to a terminal.
func stripControlChars(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}

// visibleLen returns the display width of s, ignoring ANSI SGR escape
// sequences (ESC [ ... m). Adequate for our ASCII/box-drawing content.
func visibleLen(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\033' {
			// Skip until the terminating 'm' of the SGR sequence.
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		// Treat multi-byte box-drawing runes as width 1. They arrive as
		// UTF-8; count only the leading byte of each rune.
		if s[i]&0xC0 == 0x80 {
			continue // continuation byte
		}
		n++
	}
	return n
}
