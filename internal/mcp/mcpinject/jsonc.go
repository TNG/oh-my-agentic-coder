package mcpinject

import "encoding/json"

// parseJSONC unmarshals JSON that may contain comments and trailing commas.
func parseJSONC(raw []byte, v any) error {
	return json.Unmarshal(stripJSONC(raw), v)
}

// stripJSONC removes // and /* */ comments and trailing commas outside of
// string literals.
func stripJSONC(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inString := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inString {
			out = append(out, c)
			if c == '\\' && i+1 < len(src) {
				i++
				out = append(out, src[i])
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i++
		case c == '}' || c == ']':
			out = dropTrailingComma(out)
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

func dropTrailingComma(out []byte) []byte {
	for j := len(out) - 1; j >= 0; j-- {
		switch out[j] {
		case ' ', '\t', '\r', '\n':
			continue
		case ',':
			return append(out[:j], out[j+1:]...)
		}
		break
	}
	return out
}
