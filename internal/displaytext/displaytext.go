// Package displaytext defines which characters omac removes from untrusted
// text before showing it in a terminal, a dialog or an agent's prompt.
//
// Skill names, descriptions and file paths can come from files the agent
// wrote. Without this filter such text could carry a terminal escape
// sequence that repaints the screen, a bidi override (U+202E) that makes
// a file named "invoice<U+202E>fdp.exe" display as "invoiceexe.pdf", or a
// line separator that starts a new markdown block in the agent's prompt.
//
// The same set is written out as a regex character class in the bridge
// scripts, which cannot import Go code:
//
//	.claude/hooks/omac-bridge.sh, .codex/hooks/omac-bridge.sh,
//	.copilot/hooks/omac-bridge.sh, .pi/extensions/omac-bridge.ts,
//	internal/plugin/assets/omac-multidir.ts (mirrored in .opencode/plugins/)
//
// TestBridgeCopiesMatch fails when one of them differs from Unsafe.
package displaytext

import "strings"

// Unsafe reports whether r must not reach a display: C0 and C1 control
// characters (including newline and tab), Unicode line and paragraph
// separators, the zero-width space, word joiner and byte-order mark, and
// bidi controls.
//
// The zero-width non-joiner (U+200C) and joiner (U+200D) are kept: they are
// part of correct spelling in scripts such as Persian or Hindi and of emoji
// sequences like "woman technologist", and they cannot reorder text or
// start a new line.
func Unsafe(r rune) bool {
	if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
		return true
	}
	switch r {
	case '\u2028', '\u2029', // line / paragraph separators
		'\u200b', '\u2060', '\ufeff', // zero-width space, word joiner, BOM
		'\u061c', '\u200e', '\u200f', // directional marks
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e', // bidi overrides
		'\u2066', '\u2067', '\u2068', '\u2069': // bidi isolates
		return true
	}
	return false
}

// Strip removes every Unsafe character from s. It is meant for display
// only: never use the result as a lookup key, because two different
// paths can strip to the same text.
func Strip(s string) string {
	return strings.Map(func(r rune) rune {
		if Unsafe(r) {
			return -1
		}
		return r
	}, s)
}

// UnsafeRunes lists every rune for which Unsafe is true, in ascending
// order. Tests use it to feed each character through a sanitizer.
func UnsafeRunes() []rune {
	var out []rune
	for r := rune(0); r <= 0xffff; r++ {
		if Unsafe(r) {
			out = append(out, r)
		}
	}
	return out
}
