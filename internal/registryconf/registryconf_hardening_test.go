package registryconf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// Tests for the PR #259 review findings. Each case is a shape that slipped
// through the first round: the credential correlation was keyed on a host
// spelled differently on each side, and the value expansion could smuggle a
// secret into a position nothing stripped or refused.

// setHome points os.UserHomeDir at dir. USERPROFILE is set alongside HOME
// because os.UserHomeDir reads that variable on Windows; omac ships linux and
// darwin only (.goreleaser.yaml), so this costs nothing today and keeps the
// helper honest if that ever changes.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// --- P1: credential correlation must survive port and case differences ---

func TestCorrelationMatchesPortScopedCredential(t *testing.T) {
	// The reported gap: registryURL returned a portless host while a
	// credential key kept its port, so this pair never correlated and the
	// global registry was projected without the token it needs.
	got := ScrubNPMRC([]byte(
		"registry=https://npm.acme.test:8443\n" +
			"//npm.acme.test:8443/:_authToken=T\n"))
	if len(got.KeptKeys) != 0 {
		t.Errorf("projected %v; a global registry needing auth must be refused", got.KeptKeys)
	}
	if len(got.Rejected) != 1 || !strings.Contains(got.Rejected[0].Reason, "authentication") {
		t.Fatalf("rejected = %+v, want one entry naming the auth problem", got.Rejected)
	}

	// Same host:port, but scoped — kept, and flagged.
	scoped := ScrubNPMRC([]byte(
		"@acme:registry=https://npm.acme.test:8443\n" +
			"//npm.acme.test:8443/:_authToken=T\n"))
	if len(scoped.KeptKeys) != 1 {
		t.Errorf("kept = %v, want the scoped mapping projected", scoped.KeptKeys)
	}
	if len(scoped.NeedsAuth) != 1 {
		t.Errorf("NeedsAuth = %v, want the scoped mapping flagged", scoped.NeedsAuth)
	}
}

func TestCorrelationIsCaseInsensitive(t *testing.T) {
	// url.Parse preserves host case while credential keys were lowercased,
	// so a mixed-case mapping used to escape correlation too.
	got := ScrubNPMRC([]byte(
		"registry=https://NPM.Acme.Test\n" +
			"//npm.acme.test/:_authToken=T\n"))
	if len(got.KeptKeys) != 0 || len(got.Rejected) != 1 {
		t.Errorf("kept=%v rejected=%+v; want the global mapping refused", got.KeptKeys, got.Rejected)
	}
}

func TestCorrelationNormalizesDefaultPort(t *testing.T) {
	// npm's own credential keying strips a default port; ours must agree, or
	// an explicit :443 in the mapping hides the credential.
	got := ScrubNPMRC([]byte(
		"registry=https://npm.acme.test:443\n" +
			"//npm.acme.test/:_authToken=T\n"))
	if len(got.KeptKeys) != 0 || len(got.Rejected) != 1 {
		t.Errorf("kept=%v rejected=%+v; want the global mapping refused", got.KeptKeys, got.Rejected)
	}
}

func TestCorrelationIgnoresUnrelatedHost(t *testing.T) {
	// The flip side: a credential for a different host must not suppress a
	// perfectly good mapping.
	got := ScrubNPMRC([]byte(
		"registry=https://npm.acme.test\n" +
			"//other.example/:_authToken=T\n"))
	if len(got.KeptKeys) != 1 || len(got.NeedsAuth) != 0 {
		t.Errorf("kept=%v needsAuth=%v; want the mapping projected unflagged", got.KeptKeys, got.NeedsAuth)
	}
}

// --- P2: ${VAR} may only supply the authority ---

func TestExpansionAllowedInAuthorityOnly(t *testing.T) {
	t.Setenv("OMAC_TEST_ART_HOST", "npm.acme.test")
	t.Setenv("OMAC_TEST_SECRET", "LIVESECRET")

	t.Run("host position is projected", func(t *testing.T) {
		got := ScrubNPMRC([]byte("@acme:registry=https://${OMAC_TEST_ART_HOST}/api/npm/npm/\n"))
		if want := "@acme:registry=https://npm.acme.test/api/npm/npm/\n"; string(got.Content) != want {
			t.Errorf("content = %q, want %q", got.Content, want)
		}
	})

	t.Run("path position is refused and names the variable", func(t *testing.T) {
		got := ScrubNPMRC([]byte("@acme:registry=https://npm.acme.test/api/${OMAC_TEST_SECRET}/npm/\n"))
		if strings.Contains(string(got.Content), "LIVESECRET") {
			t.Fatalf("projected an expanded secret: %q", got.Content)
		}
		if len(got.KeptKeys) != 0 {
			t.Errorf("kept = %v, want nothing projected", got.KeptKeys)
		}
		if len(got.Rejected) != 1 {
			t.Fatalf("rejected = %+v, want 1 entry", got.Rejected)
		}
		if !strings.Contains(got.Rejected[0].Reason, "OMAC_TEST_SECRET") {
			t.Errorf("reason does not name the variable: %q", got.Rejected[0].Reason)
		}
	})

	t.Run("port position is projected", func(t *testing.T) {
		t.Setenv("OMAC_TEST_PORT", "8443")
		got := ScrubNPMRC([]byte("@acme:registry=https://npm.acme.test:${OMAC_TEST_PORT}/npm/\n"))
		if want := "@acme:registry=https://npm.acme.test:8443/npm/\n"; string(got.Content) != want {
			t.Errorf("content = %q, want %q", got.Content, want)
		}
	})

	t.Run("interpolated userinfo is stripped, not projected", func(t *testing.T) {
		t.Setenv("OMAC_TEST_USER", "dev")
		t.Setenv("OMAC_TEST_PW", "hunter2")
		got := ScrubNPMRC([]byte("@acme:registry=https://${OMAC_TEST_USER}:${OMAC_TEST_PW}@npm.acme.test/npm/\n"))
		if strings.Contains(string(got.Content), "hunter2") {
			t.Fatalf("projected an interpolated credential: %q", got.Content)
		}
		if got.StrippedUserinfo != 1 {
			t.Errorf("StrippedUserinfo = %d, want 1", got.StrippedUserinfo)
		}
	})

	t.Run("a literal dollar in the path is not a placeholder", func(t *testing.T) {
		// Guards against over-refusing: os.Expand leaves a `$` that is not
		// followed by a name untouched, so this must still project.
		got := ScrubNPMRC([]byte("@acme:registry=https://npm.acme.test/api/$/npm/\n"))
		if len(got.KeptKeys) != 1 {
			t.Errorf("kept = %v (rejected %+v), want the mapping projected", got.KeptKeys, got.Rejected)
		}
	})
}

// --- P3 / P4: what counts as a credential, and where it applies ---

func TestHostScopedNonCredentialIsNotCountedAsCredential(t *testing.T) {
	// `always-auth` is a boolean, not a token: counting it made doctor claim
	// the file "holds an auth token".
	got := ScrubNPMRC([]byte(
		"@acme:registry=https://npm.acme.test\n" +
			"//npm.acme.test/:always-auth=true\n"))
	if got.DroppedCredentials != 0 {
		t.Errorf("DroppedCredentials = %d, want 0 for a boolean flag", got.DroppedCredentials)
	}
	if len(got.NeedsAuth) != 0 {
		t.Errorf("NeedsAuth = %v; a boolean flag is not a credential", got.NeedsAuth)
	}
	if len(got.KeptKeys) != 1 {
		t.Errorf("kept = %v, want the mapping projected", got.KeptKeys)
	}
}

func TestHostlessCredentialAppliesToGlobalRegistry(t *testing.T) {
	// npm applies legacy host-less `_auth`/`_password` to the default
	// registry, so they bear on the global mapping even though they name no
	// host. Pinned here because the alternative reading (correlate only
	// //host/-scoped keys) leaves the stated refusal rationale untrue.
	got := ScrubNPMRC([]byte(
		"registry=https://npm.acme.test\n" +
			"_auth=BASE64SECRET\n"))
	if len(got.KeptKeys) != 0 {
		t.Errorf("projected %v; the global registry has a credential omac cannot supply", got.KeptKeys)
	}
	if len(got.Rejected) != 1 {
		t.Fatalf("rejected = %+v, want 1 entry", got.Rejected)
	}

	// A scoped mapping is unaffected: a host-less credential says nothing
	// about a scope's own registry.
	scoped := ScrubNPMRC([]byte(
		"@acme:registry=https://npm.acme.test\n" +
			"_auth=BASE64SECRET\n"))
	if len(scoped.KeptKeys) != 1 || len(scoped.NeedsAuth) != 0 {
		t.Errorf("kept=%v needsAuth=%v; want the scoped mapping projected unflagged",
			scoped.KeptKeys, scoped.NeedsAuth)
	}
}

// --- P5 / P6 and file-shape edge cases ---

func TestScrubHandlesBOM(t *testing.T) {
	// A Windows-authored npmrc starts with a BOM, which used to glue itself
	// to the first key — landing the mapping in Dropped, where neither the
	// launch path nor doctor reports anything.
	got := ScrubNPMRC([]byte("\uFEFF@acme:registry=https://npm.acme.test\n"))
	if len(got.KeptKeys) != 1 {
		t.Fatalf("kept = %v (dropped %d, rejected %+v), want the mapping projected",
			got.KeptKeys, got.Dropped, got.Rejected)
	}
	if want := "@acme:registry=https://npm.acme.test\n"; string(got.Content) != want {
		t.Errorf("content = %q, want %q", got.Content, want)
	}
}

func TestScrubHandlesCRLF(t *testing.T) {
	got := ScrubNPMRC([]byte("@acme:registry=https://npm.acme.test\r\n//npm.acme.test/:_authToken=T\r\n"))
	if want := "@acme:registry=https://npm.acme.test\n"; string(got.Content) != want {
		t.Errorf("content = %q, want %q", got.Content, want)
	}
	if len(got.NeedsAuth) != 1 {
		t.Errorf("NeedsAuth = %v; the CR must not defeat correlation", got.NeedsAuth)
	}
}

func TestDuplicateKeyLastWins(t *testing.T) {
	got := ScrubNPMRC([]byte(
		"@acme:registry=https://first.example\n" +
			"@acme:registry=https://second.example\n"))
	if len(got.KeptKeys) != 1 {
		t.Errorf("kept = %v, want one entry (npm ini semantics: last wins)", got.KeptKeys)
	}
	if want := "@acme:registry=https://second.example\n"; string(got.Content) != want {
		t.Errorf("content = %q, want %q", got.Content, want)
	}
}

func TestProjectNPMFollowsSymlinkedConfig(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	real := filepath.Join(t.TempDir(), "npmrc.real")
	if err := os.WriteFile(real, []byte("@acme:registry=https://npm.acme.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(home, ".npmrc")); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	projs, err := Project([]string{sandboxprofile.RegistryConfigNPM}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(projs) != 1 || !projs[0].Projected() {
		t.Fatalf("projections = %+v, want one usable projection through the symlink", projs)
	}
}
