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
			name:    "entries together above the cap",
			input:   `["1-600", "601-1200"]`,
			wantErr: []string{"1000"},
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
			name:    "not a number",
			entry:   "abc",
			wantErr: []string{"abc"},
		},
		{
			name:    "empty",
			entry:   "",
			wantErr: []string{`""`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePortEntry(tt.entry)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("ParsePortEntry(%q): want error, got %v", tt.entry, got)
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
				t.Fatalf("ParsePortEntry(%q): %v", tt.entry, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ParsePortEntry(%q) = %v, want %v", tt.entry, got, tt.want)
			}
		})
	}
}
