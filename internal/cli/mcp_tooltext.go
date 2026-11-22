package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/TNG/oh-my-agentic-coder/internal/displaytext"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
)

// Limits on the server-supplied text shown for one tool.
const (
	maxToolDescRunes   = 500
	maxToolParamsRunes = 300
	maxToolParams      = 30
)

// cleanToolText makes server-supplied text safe to print on one line: every
// whitespace run becomes one space, control, escape and bidi characters are
// removed, and the result is cut to max runes ending in "...".
func cleanToolText(s string, max int) string {
	s = strings.Join(strings.Fields(displaytext.Strip(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s))), " ")
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max]) + "..."
	}
	return s
}

// toolText builds the sanitized, capped text of a tool from what the server
// sent. A tool object that does not parse yields an empty ToolText.
func toolText(t mcppolicy.ToolInfo) mcppolicy.ToolText {
	var obj struct {
		Title       any `json:"title"`
		Description any `json:"description"`
		InputSchema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []any                      `json:"required"`
		} `json:"inputSchema"`
	}
	_ = json.Unmarshal(t.Raw, &obj)
	var out mcppolicy.ToolText
	desc, _ := obj.Description.(string)
	if title, _ := obj.Title.(string); strings.TrimSpace(title) != "" {
		desc = "[" + title + "] " + desc
	}
	out.Description = cleanToolText(desc, maxToolDescRunes)
	required := map[string]bool{}
	for _, r := range obj.InputSchema.Required {
		if n, ok := r.(string); ok {
			required[n] = true
		}
	}
	names := make([]string, 0, len(obj.InputSchema.Properties))
	for n := range obj.InputSchema.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	var parts []string
	for i, n := range names {
		if i == maxToolParams {
			parts = append(parts, fmt.Sprintf("and %d more", len(names)-maxToolParams))
			break
		}
		var prop struct {
			Type any `json:"type"`
		}
		_ = json.Unmarshal(obj.InputSchema.Properties[n], &prop)
		typ := "any"
		switch v := prop.Type.(type) {
		case string:
			typ = v
		case []any:
			var ts []string
			for _, x := range v {
				if s, ok := x.(string); ok {
					ts = append(ts, s)
				}
			}
			if len(ts) > 0 {
				typ = strings.Join(ts, "|")
			}
		}
		p := n + ": " + typ
		if required[n] {
			p += " (required)"
		}
		parts = append(parts, p)
	}
	out.Params = cleanToolText(strings.Join(parts, ", "), maxToolParamsRunes)
	return out
}

func liveText(live []mcppolicy.ToolInfo) map[string]mcppolicy.ToolText {
	m := make(map[string]mcppolicy.ToolText, len(live))
	for _, t := range live {
		m[t.Name] = toolText(t)
	}
	return m
}

// printToolText prints the description and parameters of one tool, indented.
func printToolText(w func(format string, a ...any), indent string, tt mcppolicy.ToolText) {
	// Stored text was cleaned when it was written; clean again, because the
	// registry file is only as trustworthy as the disk.
	desc := cleanToolText(tt.Description, maxToolDescRunes)
	if desc == "" {
		desc = "(no description)"
	}
	w("%sdescription: %s\n", indent, desc)
	params := cleanToolText(tt.Params, maxToolParamsRunes)
	if params == "" {
		params = "(none)"
	}
	w("%sparameters:  %s\n", indent, params)
}
