package mcpinject

import (
	"regexp"
	"strings"
)

const (
	remoteUnsupported = "remote MCP servers are not supported yet"
	noAddCommand      = `cannot derive an "omac mcp add" command from this entry; write an mcp.yaml and use "omac mcp register"`
)

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// AddCommand returns the "omac mcp add" command equivalent to e. The
// arguments are copied as they are and every environment key becomes a
// --secret; values are never part of it. A remote entry yields one line
// saying remote servers are not supported.
func AddCommand(e Existing) string {
	if e.Transport != "" && e.Transport != "stdio" {
		return remoteUnsupported
	}
	if e.Command == "" {
		return noAddCommand
	}
	scope := "project"
	if e.UserLevel {
		scope = "global"
	}
	words := []string{"omac", "mcp", "add", "--scope", scope, "--harness", e.Harness}
	for _, key := range e.EnvKeys {
		words = append(words, "--secret", key)
	}
	words = append(words, e.Name, "--", e.Command)
	words = append(words, e.Args...)
	for _, w := range words {
		if hasControlChar(w) {
			return noAddCommand
		}
	}
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shellQuote(w)
	}
	return strings.Join(quoted, " ")
}

func hasControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
