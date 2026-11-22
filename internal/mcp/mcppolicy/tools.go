package mcppolicy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Limits on how much of a server's tool list ListTools reads.
const (
	maxListPages = 100
	maxListTools = 10000
)

// ToolInfo is one tool of a server's tools/list reply.
type ToolInfo struct {
	Name string
	// Raw is the tool object as the server sent it.
	Raw json.RawMessage
	// Hash is ToolHash(Raw).
	Hash string
	// ReadOnlyHint and DestructiveHint are the server's annotations, nil
	// when absent. They are the server's claims, not facts.
	ReadOnlyHint, DestructiveHint *bool
}

// ToolHash hashes the parts of a tool the model sees and acts on: name,
// title, description, inputSchema and outputSchema. Key order and every other
// field do not change it.
func ToolHash(raw json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", fmt.Errorf("tool is not a JSON object: %w", err)
	}
	var name string
	if err := json.Unmarshal(fields["name"], &name); err != nil || name == "" {
		return "", errors.New("tool has no name")
	}
	picked := map[string]any{}
	for _, key := range []string{"name", "title", "description", "inputSchema", "outputSchema"} {
		v, ok := fields[key]
		if !ok {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(v))
		dec.UseNumber()
		var decoded any
		if err := dec.Decode(&decoded); err != nil {
			return "", fmt.Errorf("tool %q: field %s: %w", name, key, err)
		}
		picked[key] = decoded
	}
	canonical, err := encodeCanonical(picked)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// ListHash hashes a whole tool list from the per-tool hashes (one quoted
// name:hash line each, sorted by name), independent of
// order.
func ListHash(perTool map[string]string) string {
	names := make([]string, 0, len(perTool))
	for n := range perTool {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(strconv.Quote(n) + ":" + perTool[n] + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// Drift is how the live tool list differs from a pin, each list sorted.
type Drift struct{ Added, Removed, Changed []string }

// Empty reports whether the live list matches the pin.
func (d Drift) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0
}

// Compare reports how live differs from pin. A nil pin reports every live
// tool as added. A name the server lists twice with different content counts
// as changed.
func Compare(p *Pin, live []ToolInfo) Drift {
	var d Drift
	seen := map[string]string{}
	for _, t := range live {
		prev, dup := seen[t.Name]
		seen[t.Name] = t.Hash
		switch {
		case dup && prev != t.Hash:
			d.Changed = append(d.Changed, t.Name)
		case dup:
		case p == nil:
			d.Added = append(d.Added, t.Name)
		default:
			pinned, ok := p.Tools[t.Name]
			if !ok {
				d.Added = append(d.Added, t.Name)
			} else if pinned != t.Hash {
				d.Changed = append(d.Changed, t.Name)
			}
		}
	}
	if p != nil {
		for name := range p.Tools {
			if _, ok := seen[name]; !ok {
				d.Removed = append(d.Removed, name)
			}
		}
	}
	d.Changed = dedupSorted(d.Changed)
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	return d
}

func dedupSorted(s []string) []string {
	sort.Strings(s)
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// Propose suggests a class from the server's annotations, which are only the
// server's claims: read-only means read, explicitly not destructive means
// write, anything else means delete. The second value says why.
func Propose(t ToolInfo) (Class, string) {
	switch {
	case t.ReadOnlyHint != nil && *t.ReadOnlyHint:
		return Read, "server marks it read-only"
	case t.DestructiveHint != nil && !*t.DestructiveHint:
		return Write, "server marks it not destructive"
	}
	return Delete, "no hint says it is safe, so it is assumed destructive"
}

// RoundTripper sends one JSON-RPC request and returns the result.
type RoundTripper interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// ListTools reads the complete tools/list of a server, following nextCursor.
// It fails when the server returns more than 100 pages or 10,000 tools, a
// tool without a name, or two tools with the same name.
func ListTools(ctx context.Context, rt RoundTripper) ([]ToolInfo, error) {
	var (
		out    []ToolInfo
		names  = map[string]bool{}
		cursor string
	)
	for page := 0; page < maxListPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := rt.Call(ctx, "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		var result struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("tools/list: bad result: %w", err)
		}
		for _, rawTool := range result.Tools {
			info, err := toolInfo(rawTool)
			if err != nil {
				return nil, err
			}
			if names[info.Name] {
				return nil, fmt.Errorf("tools/list: tool %q is listed twice", info.Name)
			}
			names[info.Name] = true
			if len(out) >= maxListTools {
				return nil, fmt.Errorf("tools/list: more than %d tools", maxListTools)
			}
			out = append(out, info)
		}
		if result.NextCursor == "" {
			return out, nil
		}
		if result.NextCursor == cursor {
			return nil, errors.New("tools/list: server repeats the same cursor")
		}
		cursor = result.NextCursor
	}
	return nil, fmt.Errorf("tools/list: more than %d pages", maxListPages)
}

func toolInfo(raw json.RawMessage) (ToolInfo, error) {
	hash, err := ToolHash(raw)
	if err != nil {
		return ToolInfo{}, fmt.Errorf("tools/list: %w", err)
	}
	var t struct {
		Name        string                     `json:"name"`
		Annotations map[string]json.RawMessage `json:"annotations"`
	}
	// An annotations value of the wrong type is ignored, not trusted.
	_ = json.Unmarshal(raw, &t)
	return ToolInfo{
		Name: t.Name, Raw: raw, Hash: hash,
		ReadOnlyHint:    boolHint(t.Annotations["readOnlyHint"]),
		DestructiveHint: boolHint(t.Annotations["destructiveHint"]),
	}, nil
}

func boolHint(raw json.RawMessage) *bool {
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return nil
	}
	return &b
}
