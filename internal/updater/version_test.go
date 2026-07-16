package updater

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b    string
		wantCmp int
		wantOK  bool
	}{
		{"1.2.3", "1.2.3", 0, true},
		{"1.2.3", "1.2.4", -1, true},
		{"1.2.4", "1.2.3", 1, true},
		{"1.3.0", "1.2.9", 1, true},
		{"2.0.0", "1.9.9", 1, true},
		{"v1.2.3", "1.2.3", 0, true},        // leading v ignored
		{"1.2.3+build.5", "1.2.3", 0, true}, // build metadata ignored
		{"1.2.3-rc1", "1.2.3", -1, true},    // pre-release below release
		{"1.2.3", "1.2.3-rc1", 1, true},
		{"1.2.3-rc1", "1.2.3-rc2", -1, true},
		{"1.2.3-2", "1.2.3-10", -1, true},          // numeric identifiers compare numerically
		{"1.2.3-rc2", "1.2.3-rc10", 1, true},       // alphanumeric identifiers compare lexically
		{"1.2.3-alpha", "1.2.3-alpha.1", -1, true}, // fewer identifiers ranks lower
		{"1.2.3-1", "1.2.3-alpha", -1, true},       // numeric below alphanumeric
		{"0.1.0-dev", "0.1.0", -1, true},
		{"1.3.0-dev", "1.2.9", 1, true}, // ahead by core even as a pre-release
		{"garbage", "1.2.3", 0, false},
		{"1.2", "1.2.0", 0, false}, // core must be three fields
		{"1.2.x", "1.2.3", 0, false},
	}
	for _, tt := range tests {
		gotCmp, gotOK := compareVersions(tt.a, tt.b)
		if gotOK != tt.wantOK || (gotOK && gotCmp != tt.wantCmp) {
			t.Errorf("compareVersions(%q, %q) = (%d, %v), want (%d, %v)",
				tt.a, tt.b, gotCmp, gotOK, tt.wantCmp, tt.wantOK)
		}
	}
}
