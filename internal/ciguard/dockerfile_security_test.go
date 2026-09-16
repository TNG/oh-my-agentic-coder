package ciguard

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// curlPipeToShell matches a `curl ... | bash|sh|tar` shape: a network fetch
// whose output is piped directly into an interpreter or archive extractor
// with no intermediate file and no checksum verification.
var curlPipeToShell = regexp.MustCompile(`curl[^|\n]*\|\s*(bash|sh|tar)\b`)

// TestSecurityDockerfileDoesNotPipeNetworkToShell asserts that Dockerfile.e2e
// does not fetch a network resource and pipe it directly into a shell (or
// tar) with no integrity check.
//
// bun's and rustup's install scripts are both fetched this way today
// (`curl ... bun.sh/install | bash`, `curl ... sh.rustup.rs | sh`): a
// compromised or MITM'd installer URL runs arbitrary code during the image
// build with no pinned checksum to catch it. Reach is dev-only (this
// Dockerfile is used only by scripts/e2e-docker.sh, from no workflow), but
// still worth fixing.
func TestSecurityDockerfileDoesNotPipeNetworkToShell(t *testing.T) {
	path := filepath.Join(repoRoot(t), "Dockerfile.e2e")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("control: %s not found: the fixture is broken, not the security property: %v", path, err)
	}

	// Control: the detector matches a synthetic known-bad line, so a
	// fixture with zero real matches isn't hiding a broken regex.
	if !curlPipeToShell.MatchString("RUN curl -fsSL https://example.com/install | bash") {
		t.Fatalf("control: the detection regex did not match a known dangerous pattern: the fixture is broken, not the security property")
	}

	matches := curlPipeToShell.FindAllString(string(raw), -1)
	if len(matches) > 0 {
		t.Errorf("Dockerfile.e2e pipes a network fetch directly into a shell/tar with no checksum verification: %v", matches)
	}
}
