package displaytext

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

func TestStrip(t *testing.T) {
	for _, r := range UnsafeRunes() {
		if got := Strip("café 東京 عربي left" + string(r) + "right"); got != "café 東京 عربي leftright" {
			t.Errorf("U+%04X: Strip = %q", r, got)
		}
	}
}

// bridgeCopies are the scripts that carry the Unsafe set as a regex
// character class, relative to the repository root.
var bridgeCopies = []string{
	".claude/hooks/omac-bridge.sh",
	".codex/hooks/omac-bridge.sh",
	".copilot/hooks/omac-bridge.sh",
	".pi/extensions/omac-bridge.ts",
	".opencode/plugins/omac-multidir.ts",
	"internal/plugin/assets/omac-multidir.ts",
}

// unsafeClass finds the character class that starts with NUL, written as
// \x00 (TypeScript) or \u0000 (jq), e.g. [\x00-\x1f\x7f-\x9f\u061c...].
var unsafeClass = regexp.MustCompile(`\[(?:\\x00|\\u0000)[^\]]*\]`)

// classItem matches one \xHH or \uHHHH escape, optionally followed by
// -\xHH or -\uHHHH to form a range.
var classItem = regexp.MustCompile(`\\(?:x([0-9a-fA-F]{2})|u([0-9a-fA-F]{4}))(?:-\\(?:x([0-9a-fA-F]{2})|u([0-9a-fA-F]{4})))?`)

// parseClass returns the runes a class like [\x00-\x1f\u061c] matches. It
// fails the test on any syntax it does not understand, so a copy cannot
// slip past the comparison by using another notation.
func parseClass(t *testing.T, class string) map[rune]bool {
	t.Helper()
	body := class[1 : len(class)-1]
	if rest := classItem.ReplaceAllString(body, ""); rest != "" {
		t.Fatalf("character class %s contains unsupported syntax %q", class, rest)
	}
	hex := func(a, b string) rune {
		if a == "" {
			a = b
		}
		v, err := strconv.ParseUint(a, 16, 32)
		if err != nil {
			t.Fatal(err)
		}
		return rune(v)
	}
	set := map[rune]bool{}
	for _, m := range classItem.FindAllStringSubmatch(body, -1) {
		lo := hex(m[1], m[2])
		hi := lo
		if m[3] != "" || m[4] != "" {
			hi = hex(m[3], m[4])
		}
		for r := lo; r <= hi; r++ {
			set[r] = true
		}
	}
	return set
}

// TestBridgeCopiesMatch fails when a bridge script strips a different set
// of characters than Unsafe. Update the class in every listed file when
// Unsafe changes.
func TestBridgeCopiesMatch(t *testing.T) {
	for _, rel := range bridgeCopies {
		t.Run(rel, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			classes := unsafeClass.FindAllString(string(src), -1)
			if len(classes) != 1 {
				t.Fatalf("want exactly one control-character class, found %d: %q", len(classes), classes)
			}
			set := parseClass(t, classes[0])
			for r := rune(0); r <= 0xffff; r++ {
				if set[r] != Unsafe(r) {
					t.Errorf("U+%04X: stripped by %s = %v, by displaytext.Unsafe = %v", r, rel, set[r], Unsafe(r))
				}
			}
		})
	}
}

// TestStripKeepsJoiners checks that U+200C and U+200D survive: removing
// them would misspell Persian text and break emoji sequences.
func TestStripKeepsJoiners(t *testing.T) {
	for _, s := range []string{
		"\u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645", // Persian "I want", with a non-joiner
		"\U0001f469\u200d\U0001f4bb",                       // woman technologist emoji
	} {
		if got := Strip(s); got != s {
			t.Errorf("Strip(%q) = %q, want it unchanged", s, got)
		}
	}
}
