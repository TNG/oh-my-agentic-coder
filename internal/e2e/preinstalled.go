//go:build e2e || e2e_fast

package e2e

import (
	"fmt"
	"os"
	"strings"
)

// preinstalledEnvVar, set to "1", makes installHarness use the harness already
// on PATH instead of installing it.
const preinstalledEnvVar = "E2E_HARNESS_PREINSTALLED"

func preinstalledEnabled() bool {
	return os.Getenv(preinstalledEnvVar) == "1"
}

// pinnedVersion returns the version in a package spec, or false when the spec
// has none or only a tag such as "@latest".
func pinnedVersion(spec string) (string, bool) {
	i := strings.LastIndexByte(spec, '@')
	if i <= 0 || i == len(spec)-1 || spec[i+1] < '0' || spec[i+1] > '9' {
		return "", false
	}
	return spec[i+1:], true
}

// checkPreinstalledVersion compares the output of `<harness> --version` with
// the version in the pinned package spec ("name@1.2.3"). A spec without a
// version (E2E_USE_LATEST) or with a tag such as "@latest" accepts any
// installed version.
func checkPreinstalledVersion(harness, spec, reported string) error {
	want, ok := pinnedVersion(spec)
	if !ok {
		return nil
	}
	if got := parseVersion(reported); got != want {
		return fmt.Errorf("preinstalled %s reports version %q, but %q is pinned (%s)", harness, got, want, spec)
	}
	return nil
}
