package updater

import (
	"context"
	"errors"
	"testing"
)

func TestSelfCheck_UpToDateNotShadowed(t *testing.T) {
	deps := Deps{
		PathLookup:   func(string) (string, error) { return "/usr/bin/omac", nil },
		VersionProbe: func(context.Context, string) (string, error) { return "1.2.0", nil },
	}
	res, err := SelfCheck(context.Background(), Plan{LatestVersion: "1.2.0"}, deps)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if res.Shadowed {
		t.Fatalf("Shadowed = true, want false (resolved binary is the installed version)")
	}
	if res.ResolvedVersion != "1.2.0" {
		t.Fatalf("ResolvedVersion = %q, want 1.2.0", res.ResolvedVersion)
	}
}

func TestSelfCheck_ShadowedByOlderBinary(t *testing.T) {
	deps := Deps{
		PathLookup:   func(string) (string, error) { return "/home/u/.local/bin/omac", nil },
		VersionProbe: func(context.Context, string) (string, error) { return "1.1.0", nil },
	}
	res, err := SelfCheck(context.Background(), Plan{LatestVersion: "1.2.0"}, deps)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !res.Shadowed {
		t.Fatalf("Shadowed = false, want true (PATH resolves to an older binary)")
	}
	if res.ResolvedPath != "/home/u/.local/bin/omac" {
		t.Fatalf("ResolvedPath = %q", res.ResolvedPath)
	}
}

// TestSelfCheck_NewerBinaryNotShadowed guards against mislabeling a *newer*
// binary earlier on PATH (e.g. a local dev build past the last release tag) as
// a stale shadow. The warning and its "rm <path>" suggestion assume the
// resolved binary is older; a newer one must never be reported as shadowed.
func TestSelfCheck_NewerBinaryNotShadowed(t *testing.T) {
	deps := Deps{
		PathLookup:   func(string) (string, error) { return "/home/u/.local/bin/omac", nil },
		VersionProbe: func(context.Context, string) (string, error) { return "1.3.0", nil },
	}
	res, err := SelfCheck(context.Background(), Plan{LatestVersion: "1.2.0"}, deps)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if res.Shadowed {
		t.Fatalf("Shadowed = true, want false (resolved binary is newer than the release)")
	}
}

// TestSelfCheck_UnparseableFallsBackToInequality keeps the conservative behavior
// when versions are not comparable as semver: any mismatch is flagged.
func TestSelfCheck_UnparseableFallsBackToInequality(t *testing.T) {
	deps := Deps{
		PathLookup:   func(string) (string, error) { return "/usr/bin/omac", nil },
		VersionProbe: func(context.Context, string) (string, error) { return "dev-abc", nil },
	}
	res, err := SelfCheck(context.Background(), Plan{LatestVersion: "1.2.0"}, deps)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !res.Shadowed {
		t.Fatalf("Shadowed = false, want true (unparseable version differs from installed)")
	}
}

func TestSelfCheck_ProbeErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	deps := Deps{
		PathLookup:   func(string) (string, error) { return "/usr/bin/omac", nil },
		VersionProbe: func(context.Context, string) (string, error) { return "", sentinel },
	}
	_, err := SelfCheck(context.Background(), Plan{LatestVersion: "1.2.0"}, deps)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestSelfCheck_StripsLeadingV(t *testing.T) {
	deps := Deps{
		PathLookup:   func(string) (string, error) { return "/usr/bin/omac", nil },
		VersionProbe: func(context.Context, string) (string, error) { return "v1.2.0", nil },
	}
	res, err := SelfCheck(context.Background(), Plan{LatestVersion: "1.2.0"}, deps)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Shadowed {
		t.Fatalf("Shadowed = true, want false; a leading v must not cause a spurious mismatch")
	}
}

func TestParseVersionOutput(t *testing.T) {
	if got := parseVersionOutput("omac 1.2.0\n"); got != "1.2.0" {
		t.Fatalf("got %q, want 1.2.0", got)
	}
	if got := parseVersionOutput(""); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
