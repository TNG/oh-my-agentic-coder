package sandboxprofile

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// maxExpandedPorts caps how many ports one range entry may expand to,
// how many ports one profile field may name, and how many the decoder
// will build before rejecting the list. Without it a typo like
// "1-65535" would silently grant every port, which defeats the purpose
// of naming ports at all.
const maxExpandedPorts = 1000

// wantPortForm ends every parse error so the message says what an entry
// looks like, not only what failed.
const wantPortForm = "want a port such as 8000 or a range such as 8000-8100"

var errNotAPortNumber = errors.New("not a decimal port number")

// PortList is a port list that also accepts inclusive range strings,
// expanded when the profile is loaded. In memory it holds plain
// numbers, so it marshals back as an integer array.
type PortList []int

// ListenPortList is network.listen_port: a PortList that names its field
// when an entry fails to decode.
type ListenPortList PortList

// AllowTCPConnectList is network.allow_tcp_connect: a PortList that names
// its field when an entry fails to decode.
type AllowTCPConnectList PortList

func (l *ListenPortList) UnmarshalJSON(data []byte) error {
	return unmarshalPortField("network.listen_port", (*PortList)(l), data)
}

func (l *AllowTCPConnectList) UnmarshalJSON(data []byte) error {
	return unmarshalPortField("network.allow_tcp_connect", (*PortList)(l), data)
}

func unmarshalPortField(field string, l *PortList, data []byte) error {
	if err := l.UnmarshalJSON(data); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

// UnmarshalJSON accepts a JSON array whose entries are integers or
// strings such as "8000" or "8100-8200". A single range entry may not
// expand beyond maxExpandedPorts, and neither may the running total
// across entries: the count is checked while the list is built, so an
// oversized profile fails before the rest of it is allocated.
func (l *PortList) UnmarshalJSON(data []byte) error {
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		raw := string(data)
		if len(raw) > 80 {
			raw = raw[:80] + "..."
		}
		return fmt.Errorf("invalid port list %s: want a JSON array", raw)
	}
	ports := make([]int, 0, len(entries))
	appendEntry := func(expanded []int, name string) error {
		if len(ports)+len(expanded) > maxExpandedPorts {
			return fmt.Errorf("invalid port or range %s: the list would exceed the %d-port limit", name, maxExpandedPorts)
		}
		ports = append(ports, expanded...)
		return nil
	}
	for _, entry := range entries {
		// A JSON null decodes into int and string without error, so it
		// has to be rejected before either attempt.
		if string(entry) == "null" {
			return fmt.Errorf("invalid port list entry %q: want an integer or a port string", entry)
		}
		var port int
		if err := json.Unmarshal(entry, &port); err == nil {
			if err := appendEntry([]int{port}, strconv.Itoa(port)); err != nil {
				return err
			}
			continue
		}
		var s string
		if err := json.Unmarshal(entry, &s); err != nil {
			return fmt.Errorf("invalid port list entry %q: want an integer or a port string", entry)
		}
		expanded, err := parsePortEntry(s)
		if err != nil {
			return err
		}
		if err := appendEntry(expanded, fmt.Sprintf("%q", s)); err != nil {
			return err
		}
	}
	*l = ports
	return nil
}

// parsePortEntry parses one port entry: a decimal port "N" or an
// inclusive range "A-B". Leading zeros are allowed, and whitespace
// around the entry and around the dash of a range is ignored, so
// " 0080 - 0085 " yields 80 through 85. Every port must be in 0-65535;
// callers enforce their own minimum.
func parsePortEntry(s string) ([]int, error) {
	entry := strings.TrimSpace(s)
	if entry == "" {
		return nil, fmt.Errorf("invalid port or range %q: empty; %s", s, wantPortForm)
	}
	start, end, isRange := strings.Cut(entry, "-")
	if !isRange {
		port, err := parsePort(entry)
		if err != nil {
			return nil, portEntryError(s, "", err)
		}
		return []int{port}, nil
	}
	lo, err := parsePort(strings.TrimSpace(start))
	if err != nil {
		return nil, portEntryError(s, "range start: ", err)
	}
	hi, err := parsePort(strings.TrimSpace(end))
	if err != nil {
		return nil, portEntryError(s, "range end: ", err)
	}
	if lo > hi {
		return nil, fmt.Errorf("invalid port or range %q: range start %d exceeds end %d; %s", s, lo, hi, wantPortForm)
	}
	if hi-lo+1 > maxExpandedPorts {
		return nil, fmt.Errorf("invalid port or range %q: range expands to %d ports, above the %d-port limit", s, hi-lo+1, maxExpandedPorts)
	}
	ports := make([]int, 0, hi-lo+1)
	for port := lo; port <= hi; port++ {
		ports = append(ports, port)
	}
	return ports, nil
}

// portEntryError words a parsePort failure in the entry syntax shared by
// profiles and flags. A bounds failure keeps its detail; a plain syntax
// failure is fully covered by the expected form.
func portEntryError(entry, side string, err error) error {
	if errors.Is(err, errNotAPortNumber) {
		return fmt.Errorf("invalid port or range %q: %s%s", entry, side, wantPortForm)
	}
	return fmt.Errorf("invalid port or range %q: %s%v; %s", entry, side, err, wantPortForm)
}

func parsePort(s string) (int, error) {
	port, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return 0, fmt.Errorf("port %s is outside 0-65535", s)
		}
		return 0, errNotAPortNumber
	}
	if port < 0 || port > 65535 {
		return 0, fmt.Errorf("port %d is outside 0-65535", port)
	}
	return int(port), nil
}
