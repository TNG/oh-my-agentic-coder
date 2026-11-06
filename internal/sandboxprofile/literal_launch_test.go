package sandboxprofile

import "testing"

func TestLiteralLaunchGrantsRejectRootAndRelativePaths(t *testing.T) {
	for _, args := range [][]string{
		{"--allow-literal", "/", "--", "true"},
		{"--allow-literal", "relative", "--", "true"},
		{"--require-protected-path", "relative", "--", "true"},
	} {
		f, err := ParseFlags(args)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := Merge(DefaultProfile(), f)
		if err := p.Validate(); err == nil {
			t.Errorf("unsafe literal launch flag accepted: %q", args)
		}
	}
}
