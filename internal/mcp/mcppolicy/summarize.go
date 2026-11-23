package mcppolicy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const valuePreviewRunes = 40

var credentialWords = []string{"token", "secret", "password", "key", "auth", "bearer", "credential"}

func credentialLike(key string) bool {
	lower := strings.ToLower(key)
	for _, w := range credentialWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// SummarizeArgs describes the top-level arguments of a call for a dialog or
// the audit log: each key with a short preview of its value, the value of a
// credential-like key masked, nested objects and arrays reduced to their
// size. The result is printable ASCII (anything else is percent-encoded) and
// at most max bytes.
func SummarizeArgs(args json.RawMessage, max int) string {
	if max <= 0 || len(bytes.TrimSpace(args)) == 0 || isNull(args) {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil {
		return capSummary("(arguments are not an object)", max)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		preview := "***"
		if !credentialLike(k) {
			preview = previewValue(obj[k])
		}
		parts = append(parts, asciiOnly(k)+"="+preview)
	}
	return capSummary(strings.Join(parts, ", "), max)
}

func previewValue(raw json.RawMessage) string {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "?"
	}
	switch x := v.(type) {
	case string:
		return `"` + asciiOnly(truncateRunes(x, valuePreviewRunes)) + `"`
	case map[string]any:
		return fmt.Sprintf("{%d keys}", len(x))
	case []any:
		return fmt.Sprintf("[%d items]", len(x))
	case nil:
		return "null"
	case bool:
		return fmt.Sprint(x)
	case json.Number:
		return truncateRunes(x.String(), valuePreviewRunes)
	}
	return "?"
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}

// asciiOnly percent-encodes every byte outside printable ASCII, and '%'
// itself so the encoding is unambiguous.
func asciiOnly(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e || c == '%' {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// capSummary shortens s to max bytes, ending in "..." and never inside a
// percent escape.
func capSummary(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "..."
	if max <= len(ellipsis) {
		return ellipsis[:max]
	}
	cut := s[:max-len(ellipsis)]
	if i := strings.LastIndexByte(cut, '%'); i >= 0 && len(cut)-i < 3 {
		cut = cut[:i]
	}
	return cut + ellipsis
}
