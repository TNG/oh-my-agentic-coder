package mcppolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const maxJSONDepth = 64

// Keys whose spelling must be exact. Go's decoder matches struct fields
// case-insensitively and keeps the last duplicate, so a case variant could
// make the facade and the server disagree about what the message says.
var (
	topLevelKeys = []string{"jsonrpc", "id", "method", "params", "result", "error"}
	paramsKeys   = []string{"name", "arguments", "_meta", "cursor"}
)

// ParseRequest reads one JSON-RPC message from the agent and fails closed on
// anything the facade and the MCP server might read differently: batches,
// non-objects, trailing data, invalid UTF-8, duplicate object keys at any
// depth, case variants of the keys that matter, and a params.name that is
// not a string. Besides the Call it returns the canonical re-encoding of the
// same message, which is what the facade forwards.
func ParseRequest(body []byte) (Call, []byte, error) {
	var zero Call
	if !utf8.Valid(body) {
		return zero, nil, errors.New("message is not valid UTF-8")
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return zero, nil, errors.New("message is empty")
	}
	switch trimmed[0] {
	case '[':
		return zero, nil, errors.New("JSON-RPC batches are not supported")
	case '{':
	default:
		return zero, nil, errors.New("message is not a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := checkValue(dec, 0); err != nil {
		return zero, nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return zero, nil, errors.New("unexpected data after the JSON message")
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return zero, nil, fmt.Errorf("decode message: %w", err)
	}
	if err := checkExactKeys(top, topLevelKeys, "message"); err != nil {
		return zero, nil, err
	}
	call, err := callFromFields(top)
	if err != nil {
		return zero, nil, err
	}
	canonical, err := encodeCanonical(top)
	if err != nil {
		return zero, nil, err
	}
	return call, canonical, nil
}

func callFromFields(top map[string]json.RawMessage) (Call, error) {
	var c Call
	if id, ok := top["id"]; ok {
		if err := checkID(id); err != nil {
			return Call{}, err
		}
		c.ID = id
	}
	rawMethod, hasMethod := top["method"]
	if !hasMethod {
		_, hasResult := top["result"]
		_, hasError := top["error"]
		if (hasResult || hasError) && c.ID != nil {
			c.Response = true
			return c, nil
		}
		return Call{}, errors.New("message has no method")
	}
	if err := json.Unmarshal(rawMethod, &c.Method); err != nil || c.Method == "" {
		return Call{}, errors.New("method must be a non-empty string")
	}
	rawParams, hasParams := top["params"]
	var params map[string]json.RawMessage
	if hasParams && !isNull(rawParams) {
		if err := json.Unmarshal(rawParams, &params); err != nil {
			if c.Method == "tools/call" {
				return Call{}, errors.New("params must be an object")
			}
			return c, nil
		}
		if err := checkExactKeys(params, paramsKeys, "params"); err != nil {
			return Call{}, err
		}
	}
	if c.Method != "tools/call" {
		return c, nil
	}
	rawName, ok := params["name"]
	if !ok {
		return Call{}, errors.New("tools/call has no params.name")
	}
	if err := json.Unmarshal(rawName, &c.Tool); err != nil {
		return Call{}, errors.New("params.name must be a string")
	}
	if c.Tool == "" || strings.ContainsRune(c.Tool, utf8.RuneError) {
		return Call{}, errors.New("params.name must be a non-empty string without invalid characters")
	}
	if args, ok := params["arguments"]; ok && !isNull(args) {
		if !isObject(args) {
			return Call{}, errors.New("params.arguments must be an object")
		}
		c.Args = args
	}
	return c, nil
}

func checkID(id json.RawMessage) error {
	if isNull(id) || len(id) > 0 && (id[0] == '"' || id[0] == '-' || id[0] >= '0' && id[0] <= '9') {
		return nil
	}
	return errors.New("id must be a string, a number or null")
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

func isObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{'
}

// checkExactKeys refuses a key that equals one of names ignoring case but not
// exactly.
func checkExactKeys(m map[string]json.RawMessage, names []string, where string) error {
	for key := range m {
		for _, name := range names {
			if key != name && strings.EqualFold(key, name) {
				return fmt.Errorf("%s has key %q, which differs from %q only in case", where, key, name)
			}
		}
	}
	return nil
}

// checkValue consumes one JSON value from dec and refuses duplicate keys in
// every object inside it.
func checkValue(dec *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return errors.New("message is nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return fmt.Errorf("invalid JSON: %w", err)
			}
			key, _ := keyTok.(string)
			if seen[key] {
				return fmt.Errorf("duplicate key %q", key)
			}
			seen[key] = true
			if err := checkValue(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := checkValue(dec, depth+1); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// encodeCanonical writes the message with sorted top-level keys and compact
// values, without HTML escaping, so the bytes differ from the input only in
// layout.
func encodeCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode message: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
