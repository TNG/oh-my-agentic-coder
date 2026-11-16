package cli

import (
	"strings"
	"testing"
)

func TestDesktopStateHome(t *testing.T) {
	for _, tc := range []struct {
		raw, want, wantErr string
	}{
		{raw: "/home/a/.local/state", want: "/home/a/.local/state"},
		{raw: "/home/a/.local/state/", want: "/home/a/.local/state"},
		{raw: "/home/a/x/../state", want: "/home/a/state"},
		{raw: "relative/dir", wantErr: `XDG_STATE_HOME="relative/dir" must be an absolute path`},
		{raw: "", wantErr: `XDG_STATE_HOME="" must be an absolute path`},
	} {
		got, err := desktopStateHome(tc.raw)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("desktopStateHome(%q) error = %v, want %q", tc.raw, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("desktopStateHome(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
}
