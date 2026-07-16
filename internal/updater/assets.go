package updater

import "strings"

// matchAsset finds the release asset for goos/goarch whose name ends with
// formatSuffix. It scans by substring/suffix rather than reconstructing the
// goreleaser filename template, so it keeps working if that template changes.
func matchAsset(assets []Asset, goos, goarch, formatSuffix string) (Asset, bool) {
	osTokens := osTokensFor(goos)
	archTokens := archTokensFor(goarch)
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		if !strings.HasSuffix(name, formatSuffix) {
			continue
		}
		if !containsAny(name, osTokens) || !containsAny(name, archTokens) {
			continue
		}
		return a, true
	}
	return Asset{}, false
}

func osTokensFor(goos string) []string {
	switch goos {
	case "darwin":
		return []string{"macos", "darwin"}
	case "linux":
		return []string{"linux"}
	default:
		return []string{goos}
	}
}

func archTokensFor(goarch string) []string {
	switch goarch {
	case "amd64":
		return []string{"x86_64", "amd64"}
	case "arm64":
		return []string{"arm64", "aarch64"}
	default:
		return []string{goarch}
	}
}

func containsAny(s string, tokens []string) bool {
	for _, t := range tokens {
		if strings.Contains(s, strings.ToLower(t)) {
			return true
		}
	}
	return false
}
