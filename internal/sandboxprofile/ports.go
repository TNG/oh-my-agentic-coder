package sandboxprofile

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// maxExpandedPorts caps how many ports a single list may expand to.
// Without it a typo like "1-65535" would silently grant every port,
// which defeats the purpose of naming ports at all.
const maxExpandedPorts = 1000

// PortList is a port list that also accepts inclusive range strings,
// expanded when the profile is loaded. In memory it holds plain
// numbers, so it marshals back as an integer array.
type PortList []int

// UnmarshalJSON accepts a JSON array whose entries are integers or
// strings such as "8000" or "8100-8200". The expanded list may not
// exceed maxExpandedPorts.
func (l *PortList) UnmarshalJSON(data []byte) error {
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("invalid port list %s: want a JSON array", data)
	}
	ports := make([]int, 0, len(entries))
	for _, entry := range entries {
		// A JSON null decodes into int and string without error, so it
		// has to be rejected before either attempt.
		if string(entry) == "null" {
			return fmt.Errorf("invalid port list entry %q: want an integer or a port string", entry)
		}
		var port int
		if err := json.Unmarshal(entry, &port); err == nil {
			ports = append(ports, port)
			continue
		}
		var s string
		if err := json.Unmarshal(entry, &s); err != nil {
			return fmt.Errorf("invalid port list entry %q: want an integer or a port string", entry)
		}
		expanded, err := ParsePortEntry(s)
		if err != nil {
			return err
		}
		ports = append(ports, expanded...)
	}
	if len(ports) > maxExpandedPorts {
		return fmt.Errorf("port list expands to %d ports, above the %d-port limit", len(ports), maxExpandedPorts)
	}
	*l = ports
	return nil
}

// ParsePortEntry parses one port entry: a decimal port "N" or an
// inclusive range "A-B". Surrounding whitespace is ignored and leading
// zeros are allowed, so " 0080-0085 " yields 80 through 85. Every port
// must be in 0-65535; callers enforce their own minimum.
func ParsePortEntry(s string) ([]int, error) {
	entry := strings.TrimSpace(s)
	if entry == "" {
		return nil, fmt.Errorf("invalid port list entry %q: empty", s)
	}
	start, end, isRange := strings.Cut(entry, "-")
	if !isRange {
		port, err := parsePort(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid port list entry %q: %w", s, err)
		}
		return []int{port}, nil
	}
	lo, err := parsePort(start)
	if err != nil {
		return nil, fmt.Errorf("invalid port list entry %q: range start: %w", s, err)
	}
	hi, err := parsePort(end)
	if err != nil {
		return nil, fmt.Errorf("invalid port list entry %q: range end: %w", s, err)
	}
	if lo > hi {
		return nil, fmt.Errorf("invalid port list entry %q: range start %d exceeds end %d", s, lo, hi)
	}
	if hi-lo+1 > maxExpandedPorts {
		return nil, fmt.Errorf("invalid port list entry %q: range expands to %d ports, above the %d-port limit", s, hi-lo+1, maxExpandedPorts)
	}
	ports := make([]int, 0, hi-lo+1)
	for port := lo; port <= hi; port++ {
		ports = append(ports, port)
	}
	return ports, nil
}

func parsePort(s string) (int, error) {
	port, err := strconv.Atoi(s)
	if err != nil {
		return 0, errors.New("not a decimal port number")
	}
	if port < 0 || port > 65535 {
		return 0, fmt.Errorf("port %d is outside 0-65535", port)
	}
	return port, nil
}
