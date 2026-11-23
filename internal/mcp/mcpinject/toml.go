package mcpinject

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// tomlTable is a parsed TOML table. A value is a string, a []any, a
// tomlTable, or tomlScalar for numbers, booleans and dates. The parser is
// strict about syntax and keeps no more of the document than the scan reads.
// go.mod has no TOML library and the scan reads only a few keys, so a small
// parser avoids a new dependency.
type tomlTable map[string]any

type tomlScalar string

type tomlParser struct {
	src  string
	pos  int
	root tomlTable
	cur  tomlTable
}

func parseTOML(raw []byte) (tomlTable, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: not valid UTF-8", errParse)
	}
	p := &tomlParser{src: strings.ReplaceAll(string(raw), "\r\n", "\n"), root: tomlTable{}}
	p.cur = p.root
	if err := p.document(); err != nil {
		return nil, fmt.Errorf("%w: toml line %d: %v", errParse, p.line(), err)
	}
	return p.root, nil
}

func (p *tomlParser) line() int {
	return 1 + strings.Count(p.src[:min(p.pos, len(p.src))], "\n")
}

func (p *tomlParser) eof() bool { return p.pos >= len(p.src) }

func (p *tomlParser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.src[p.pos]
}

func (p *tomlParser) skipBlanks() {
	for !p.eof() && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *tomlParser) skipComment() {
	if p.peek() == '#' {
		for !p.eof() && p.src[p.pos] != '\n' {
			p.pos++
		}
	}
}

// skipSpace skips blanks, comments and newlines, as allowed inside arrays.
func (p *tomlParser) skipSpace() {
	for {
		p.skipBlanks()
		p.skipComment()
		if p.peek() != '\n' {
			return
		}
		p.pos++
	}
}

func (p *tomlParser) endOfLine() error {
	p.skipBlanks()
	p.skipComment()
	if !p.eof() && p.peek() != '\n' {
		return fmt.Errorf("unexpected %q after value", p.peek())
	}
	return nil
}

func (p *tomlParser) document() error {
	for {
		p.skipSpace()
		if p.eof() {
			return nil
		}
		var err error
		if p.peek() == '[' {
			err = p.header()
		} else {
			err = p.keyValue(p.cur)
		}
		if err == nil {
			err = p.endOfLine()
		}
		if err != nil {
			return err
		}
	}
}

func (p *tomlParser) header() error {
	p.pos++
	array := p.peek() == '['
	if array {
		p.pos++
	}
	path, err := p.keyPath()
	if err != nil {
		return err
	}
	for _, closer := range strings.Repeat("]", 1+boolToInt(array)) {
		if p.peek() != byte(closer) {
			return fmt.Errorf("table header is not closed")
		}
		p.pos++
	}
	table, err := descend(p.root, path[:len(path)-1])
	if err != nil {
		return err
	}
	last := path[len(path)-1]
	switch existing := table[last].(type) {
	case nil:
		fresh := tomlTable{}
		if array {
			table[last] = []any{fresh}
		} else {
			table[last] = fresh
		}
		p.cur = fresh
	case tomlTable:
		if array {
			return fmt.Errorf("%q is a table, not an array of tables", last)
		}
		p.cur = existing
	case []any:
		if !array {
			return fmt.Errorf("%q is an array, not a table", last)
		}
		fresh := tomlTable{}
		table[last] = append(existing, fresh)
		p.cur = fresh
	default:
		return fmt.Errorf("%q is already a value", last)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// descend follows path from table, creating tables, and enters the last
// element of an array of tables.
func descend(table tomlTable, path []string) (tomlTable, error) {
	for _, key := range path {
		switch next := table[key].(type) {
		case nil:
			fresh := tomlTable{}
			table[key] = fresh
			table = fresh
		case tomlTable:
			table = next
		case []any:
			last, ok := next[len(next)-1].(tomlTable)
			if !ok {
				return nil, fmt.Errorf("%q is an array of values", key)
			}
			table = last
		default:
			return nil, fmt.Errorf("%q is already a value", key)
		}
	}
	return table, nil
}

func (p *tomlParser) keyValue(table tomlTable) error {
	path, err := p.keyPath()
	if err != nil {
		return err
	}
	if p.peek() != '=' {
		return fmt.Errorf("expected = after key")
	}
	p.pos++
	p.skipBlanks()
	value, err := p.value()
	if err != nil {
		return err
	}
	holder, err := descend(table, path[:len(path)-1])
	if err != nil {
		return err
	}
	holder[path[len(path)-1]] = value
	return nil
}

func (p *tomlParser) keyPath() ([]string, error) {
	var path []string
	for {
		p.skipBlanks()
		key, err := p.key()
		if err != nil {
			return nil, err
		}
		path = append(path, key)
		p.skipBlanks()
		if p.peek() != '.' {
			return path, nil
		}
		p.pos++
	}
}

func isBareKeyByte(c byte) bool {
	return c == '-' || c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func (p *tomlParser) key() (string, error) {
	switch p.peek() {
	case '"':
		return p.basicString()
	case '\'':
		return p.literalString()
	}
	start := p.pos
	for !p.eof() && isBareKeyByte(p.src[p.pos]) {
		p.pos++
	}
	if start == p.pos {
		return "", fmt.Errorf("expected a key")
	}
	return p.src[start:p.pos], nil
}

func (p *tomlParser) value() (any, error) {
	switch p.peek() {
	case '"':
		if strings.HasPrefix(p.src[p.pos:], `"""`) {
			return p.multilineBasic()
		}
		return p.basicString()
	case '\'':
		if strings.HasPrefix(p.src[p.pos:], `'''`) {
			return p.multilineLiteral()
		}
		return p.literalString()
	case '[':
		return p.array()
	case '{':
		return p.inlineTable()
	}
	start := p.pos
	for !p.eof() && !strings.ContainsRune(",]}#\n", rune(p.src[p.pos])) {
		p.pos++
	}
	token := strings.TrimSpace(p.src[start:p.pos])
	if token == "" {
		return nil, fmt.Errorf("missing value")
	}
	if strings.ContainsAny(token, "\"'=[{") {
		return nil, fmt.Errorf("invalid value %q", token)
	}
	return tomlScalar(token), nil
}

func (p *tomlParser) array() (any, error) {
	p.pos++
	items := []any{}
	for {
		p.skipSpace()
		if p.peek() == ']' {
			p.pos++
			return items, nil
		}
		item, err := p.value()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		p.skipSpace()
		switch p.peek() {
		case ',':
			p.pos++
		case ']':
		default:
			return nil, fmt.Errorf("expected , or ] in array")
		}
	}
}

func (p *tomlParser) inlineTable() (any, error) {
	p.pos++
	table := tomlTable{}
	p.skipBlanks()
	if p.peek() == '}' {
		p.pos++
		return table, nil
	}
	for {
		if err := p.keyValue(table); err != nil {
			return nil, err
		}
		p.skipBlanks()
		switch p.peek() {
		case ',':
			p.pos++
			p.skipBlanks()
		case '}':
			p.pos++
			return table, nil
		default:
			return nil, fmt.Errorf("expected , or } in inline table")
		}
	}
}

func (p *tomlParser) literalString() (string, error) {
	end := strings.IndexAny(p.src[p.pos+1:], "'\n")
	if end < 0 || p.src[p.pos+1+end] != '\'' {
		return "", fmt.Errorf("unterminated literal string")
	}
	out := p.src[p.pos+1 : p.pos+1+end]
	p.pos += end + 2
	return out, nil
}

func (p *tomlParser) multilineLiteral() (string, error) {
	p.pos += 3
	end := strings.Index(p.src[p.pos:], "'''")
	if end < 0 {
		return "", fmt.Errorf("unterminated multi-line literal string")
	}
	out := strings.TrimPrefix(p.src[p.pos:p.pos+end], "\n")
	p.pos += end + 3
	for strings.HasPrefix(p.src[p.pos:], "'") {
		out += "'"
		p.pos++
	}
	return out, nil
}

func (p *tomlParser) basicString() (string, error) {
	p.pos++
	return p.escaped(false)
}

func (p *tomlParser) multilineBasic() (string, error) {
	p.pos += 3
	if p.peek() == '\n' {
		p.pos++
	}
	return p.escaped(true)
}

// escaped reads a basic string body up to its closing quote or, for the
// multi-line form, closing triple quote.
func (p *tomlParser) escaped(multiline bool) (string, error) {
	var out strings.Builder
	for !p.eof() {
		c := p.src[p.pos]
		switch {
		case multiline && strings.HasPrefix(p.src[p.pos:], `"""`):
			p.pos += 3
			for p.peek() == '"' {
				out.WriteByte('"')
				p.pos++
			}
			return out.String(), nil
		case !multiline && c == '"':
			p.pos++
			return out.String(), nil
		case c == '\n' && !multiline:
			return "", fmt.Errorf("unterminated string")
		case c == '\\':
			if err := p.escape(&out, multiline); err != nil {
				return "", err
			}
		default:
			out.WriteByte(c)
			p.pos++
		}
	}
	return "", fmt.Errorf("unterminated string")
}

func (p *tomlParser) escape(out *strings.Builder, multiline bool) error {
	p.pos++
	if p.eof() {
		return fmt.Errorf("unterminated escape")
	}
	c := p.src[p.pos]
	p.pos++
	simple := map[byte]string{'b': "\b", 't': "\t", 'n': "\n", 'f': "\f", 'r': "\r", '"': `"`, '\\': `\`, 'e': "\x1b"}
	if text, ok := simple[c]; ok {
		out.WriteString(text)
		return nil
	}
	digits := map[byte]int{'x': 2, 'u': 4, 'U': 8}[c]
	if digits > 0 {
		if p.pos+digits > len(p.src) {
			return fmt.Errorf("short unicode escape")
		}
		code, err := strconv.ParseUint(p.src[p.pos:p.pos+digits], 16, 32)
		if err != nil || !utf8.ValidRune(rune(code)) {
			return fmt.Errorf("invalid unicode escape")
		}
		out.WriteRune(rune(code))
		p.pos += digits
		return nil
	}
	if multiline && (c == '\n' || c == ' ' || c == '\t') {
		p.pos--
		p.skipBlanks()
		if p.peek() != '\n' {
			return fmt.Errorf("invalid escape")
		}
		for !p.eof() && strings.ContainsRune(" \t\n", rune(p.src[p.pos])) {
			p.pos++
		}
		return nil
	}
	return fmt.Errorf("invalid escape \\%c", c)
}
