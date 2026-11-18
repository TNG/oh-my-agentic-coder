package sandboxprofile

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func portSeq(lo, hi int) []int {
	ports := make([]int, 0, hi-lo+1)
	for p := lo; p <= hi; p++ {
		ports = append(ports, p)
	}
	return ports
}

func TestPortListUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []int
		wantErr []string
	}{
		{
			name:  "integers only",
			input: `[4097]`,
			want:  []int{4097},
		},
		{
			name:  "number and range mixed",
			input: `[4097, "8000-8100"]`,
			want:  append([]int{4097}, portSeq(8000, 8100)...),
		},
		{
			name:  "single port as string",
			input: `["8000"]`,
			want:  []int{8000},
		},
		{
			name:  "zero is legal at this layer",
			input: `[0]`,
			want:  []int{0},
		},
		{
			name:  "empty list",
			input: `[]`,
			want:  nil,
		},
		{
			name:  "entries together at the cap",
			input: `["1-500", "501-1000"]`,
			want:  portSeq(1, 1000),
		},
		{
			name:    "reversed range",
			input:   `["8100-8000"]`,
			wantErr: []string{`"8100-8000"`},
		},
		{
			name:    "endpoint above 65535",
			input:   `["8000-70000"]`,
			wantErr: []string{`"8000-70000"`, "65535"},
		},
		{
			name:    "fractional number",
			input:   `[8000.5]`,
			wantErr: []string{`"8000.5"`},
		},
		{
			name:    "bool entry",
			input:   `[true]`,
			wantErr: []string{`"true"`},
		},
		{
			name:    "null entry",
			input:   `[null]`,
			wantErr: []string{`"null"`},
		},
		{
			name:    "not an array",
			input:   `8000`,
			wantErr: []string{"8000"},
		},
		{
			name:    "string field value",
			input:   `"8000-8100"`,
			wantErr: []string{`invalid port list "8000-8100": want a JSON array`},
		},
		{
			name:    "long value is truncated",
			input:   `"` + strings.Repeat("x", 200) + `"`,
			wantErr: []string{"want a JSON array", "..."},
		},
		{
			name:    "empty string entry",
			input:   `[""]`,
			wantErr: []string{`""`},
		},
		{
			name:    "range above the cap",
			input:   `["1-65535"]`,
			wantErr: []string{`"1-65535"`, "1000"},
		},
		{
			name:    "2000-port range above the cap",
			input:   `["1-2000"]`,
			wantErr: []string{`"1-2000"`, "1000"},
		},
		{
			name:    "string port above the int64 range",
			input:   `["99999999999999999999"]`,
			wantErr: []string{`"99999999999999999999"`, "65535"},
		},
		{
			name:    "multi-entry total above the cap",
			input:   `["1-600", "601-1200"]`,
			wantErr: []string{`"601-1200"`, "1000-port limit"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got PortList
			err := json.Unmarshal([]byte(tt.input), &got)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("unmarshal %s: want error, got %v", tt.input, got)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name %q", err, want)
					}
				}
				if strings.Contains(err.Error(), "sandbox profile:") {
					t.Errorf("error %q carries a %q prefix; the profile loader adds it", err, "sandbox profile:")
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal %s: %v", tt.input, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("unmarshal %s = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParsePortEntry(t *testing.T) {
	tests := []struct {
		name    string
		entry   string
		want    []int
		wantErr []string
	}{
		{
			name:  "range with surrounding spaces",
			entry: " 8000-8100 ",
			want:  portSeq(8000, 8100),
		},
		{
			name:  "single port",
			entry: "8000",
			want:  []int{8000},
		},
		{
			name:  "zero",
			entry: "0",
			want:  []int{0},
		},
		{
			name:  "leading zeros in range",
			entry: "0080-0085",
			want:  portSeq(80, 85),
		},
		{
			name:  "single port with leading plus sign",
			entry: "+80",
			want:  []int{80},
		},
		{
			name:  "range end with leading plus sign",
			entry: "8000-+8100",
			want:  portSeq(8000, 8100),
		},
		{
			name:  "spaces around the dash",
			entry: "8000 - 8100",
			want:  portSeq(8000, 8100),
		},
		{
			name:  "single-port range",
			entry: "8000-8000",
			want:  []int{8000},
		},
		{
			name:  "range at the cap",
			entry: "1-1000",
			want:  portSeq(1, 1000),
		},
		{
			name:    "reversed range",
			entry:   "8100-8000",
			wantErr: []string{"8100-8000"},
		},
		{
			name:    "range above the cap",
			entry:   "1-2000",
			wantErr: []string{"1-2000", "1000"},
		},
		{
			name:    "endpoint above 65535",
			entry:   "8000-70000",
			wantErr: []string{"8000-70000", "65535"},
		},
		{
			name:    "single port above 65535",
			entry:   "70000",
			wantErr: []string{"70000", "65535"},
		},
		{
			name:    "port above the int64 range",
			entry:   "99999999999999999999",
			wantErr: []string{"99999999999999999999", "outside 0-65535"},
		},
		{
			name:    "range endpoint above the int64 range",
			entry:   "8000-99999999999999999999",
			wantErr: []string{"8000-99999999999999999999", "range end", "outside 0-65535"},
		},
		{
			name:    "not a number",
			entry:   "abc",
			wantErr: []string{`invalid port or range "abc"`, "want a port such as 8000 or a range such as 8000-8100"},
		},
		{
			name:    "too many dashes",
			entry:   "80-90-100",
			wantErr: []string{`invalid port or range "80-90-100"`, "want a port such as 8000 or a range such as 8000-8100"},
		},
		{
			name:    "empty",
			entry:   "",
			wantErr: []string{`""`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePortEntry(tt.entry)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("parsePortEntry(%q): want error, got %v", tt.entry, got)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name %q", err, want)
					}
				}
				if strings.Contains(err.Error(), "sandbox profile:") {
					t.Errorf("error %q carries a %q prefix; the caller adds it", err, "sandbox profile:")
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePortEntry(%q): %v", tt.entry, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("parsePortEntry(%q) = %v, want %v", tt.entry, got, tt.want)
			}
		})
	}
}

// A PortList holds plain numbers, so a learn-mode profile rewrite writes
// the expanded ports, not the range that was read.
func TestPortListMarshalExpanded(t *testing.T) {
	got, err := json.Marshal(PortList{4097, 8000, 8001, 8002})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != `[4097,8000,8001,8002]` {
		t.Errorf("marshal = %s, want [4097,8000,8001,8002]", got)
	}
}

func FuzzParsePortEntry(f *testing.F) {
	for _, seed := range []string{
		"8000", " 8000-8100 ", "0080-0085", "0", "0-100", "1-1000",
		"8100-8000", "1-65535", "70000", "8000-70000", "8000.5",
		"99999999999999999999", "abc", "", "8000-",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, entry string) {
		ports, err := parsePortEntry(entry)
		if err != nil {
			return
		}
		if len(ports) == 0 || len(ports) > maxExpandedPorts {
			t.Fatalf("parsePortEntry(%q) returned %d ports", entry, len(ports))
		}
		for _, port := range ports {
			if port < 0 || port > 65535 {
				t.Fatalf("parsePortEntry(%q) returned port %d", entry, port)
			}
		}
	})
}
