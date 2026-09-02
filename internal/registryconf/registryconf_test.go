package registryconf

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestProjectorsCoverProfileTools guards against drift: every ecosystem
// the profile accepts must have a projector, and vice versa.
func TestProjectorsCoverProfileTools(t *testing.T) {
	tools := sandboxprofile.RegistryConfigTools()
	if len(tools) != len(projectors) {
		t.Fatalf("registry_config: %d profile tool(s) vs %d projector(s)", len(tools), len(projectors))
	}
	for _, tool := range tools {
		if _, ok := projectors[tool]; !ok {
			t.Errorf("no projector for profile tool %q", tool)
		}
	}
}

func TestScrubNPMRCKeepsOnlyRegistryMappings(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		wantKeys []string
		wantBody string
		dropped  int
		rejected int
	}{
		{
			name:     "the #241 shape: bare scope mapping",
			src:      "@tngtech:registry=https://tng-artifacts.int.tngtech.com\n",
			wantKeys: []string{"@tngtech:registry"},
			wantBody: "@tngtech:registry=https://tng-artifacts.int.tngtech.com\n",
		},
		{
			name: "token lines are dropped, mapping survives",
			src: "@acme:registry=https://npm.acme.test\n" +
				"//npm.acme.test/:_authToken=SECRET\n" +
				"_auth=BASE64SECRET\n" +
				"_password=hunter2\n" +
				"email=dev@acme.test\n",
			wantKeys: []string{"@acme:registry"},
			wantBody: "@acme:registry=https://npm.acme.test\n",
			dropped:  4,
		},
		{
			name:     "global registry mapping is kept",
			src:      "registry=https://npm.acme.test\n",
			wantKeys: []string{"registry"},
			wantBody: "registry=https://npm.acme.test\n",
		},
		{
			name:     "comments and blanks are ignored, not counted",
			src:      "; a comment\n# another\n\nregistry=https://npm.acme.test\n",
			wantKeys: []string{"registry"},
			wantBody: "registry=https://npm.acme.test\n",
		},
		{
			name:     "unrelated knobs are dropped",
			src:      "strict-ssl=false\ncafile=/etc/ca.pem\nregistry=https://npm.acme.test\n",
			wantKeys: []string{"registry"},
			wantBody: "registry=https://npm.acme.test\n",
			dropped:  2,
		},
		{
			name:     "a host containing \"token\" is a legitimate registry",
			src:      "@tt:registry=https://api.trustedtokens.eu\n",
			wantKeys: []string{"@tt:registry"},
			wantBody: "@tt:registry=https://api.trustedtokens.eu\n",
		},
		{
			name:     "non-URL mapping value is rejected, not silently dropped",
			src:      "registry=not-a-url\n",
			wantKeys: nil,
			rejected: 1,
		},
		{
			name:     "non-http scheme is rejected",
			src:      "registry=file:///tmp/evil\n",
			wantKeys: nil,
			rejected: 1,
		},
		{
			name:     "line without = is dropped",
			src:      "garbage\n",
			wantKeys: nil,
			dropped:  1,
		},
		{
			name:     "keys are matched case-insensitively",
			src:      "@Acme:Registry=https://npm.acme.test\n",
			wantKeys: []string{"@Acme:Registry"},
			wantBody: "@Acme:Registry=https://npm.acme.test\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScrubNPMRC([]byte(tt.src))
			if strings.Join(got.KeptKeys, ",") != strings.Join(tt.wantKeys, ",") {
				t.Errorf("kept keys = %v, want %v", got.KeptKeys, tt.wantKeys)
			}
			if string(got.Content) != tt.wantBody {
				t.Errorf("content = %q, want %q", got.Content, tt.wantBody)
			}
			if got.Dropped != tt.dropped {
				t.Errorf("dropped = %d, want %d", got.Dropped, tt.dropped)
			}
			if len(got.Rejected) != tt.rejected {
				t.Errorf("rejected = %d (%+v), want %d", len(got.Rejected), got.Rejected, tt.rejected)
			}
		})
	}
}

func TestScrubNPMRCStripsInlineCredentials(t *testing.T) {
	got := ScrubNPMRC([]byte("@acme:registry=https://user:hunter2@npm.acme.test/path\n"))
	if got.StrippedUserinfo != 1 {
		t.Errorf("StrippedUserinfo = %d, want 1", got.StrippedUserinfo)
	}
	if strings.Contains(string(got.Content), "hunter2") || strings.Contains(string(got.Content), "user") {
		t.Fatalf("projection leaked inline credentials: %q", got.Content)
	}
	if want := "@acme:registry=https://npm.acme.test/path\n"; string(got.Content) != want {
		t.Errorf("content = %q, want %q", got.Content, want)
	}
}

// TestScrubNPMRCNeverLeaksSecretMaterial is the invariant that justifies
// projecting a protected file at all: whatever the input, every output
// line must be a registry mapping whose URL carries no userinfo. The check
// is structural rather than textual because a legitimate key (@acme:registry)
// and a legitimate host (api.trustedtokens.eu) both look credential-ish.
func TestScrubNPMRCNeverLeaksSecretMaterial(t *testing.T) {
	credentialKey := regexp.MustCompile(`(?i)(_auth|authtoken|_password|passwd|username|email|^//)`)
	inputs := []string{
		"//registry.npmjs.org/:_authToken=npm_LIVETOKEN\n@acme:registry=https://npm.acme.test\n",
		"_auth=aGVsbG86d29ybGQ=\n_password=pw\nusername=dev\nregistry=https://npm.acme.test\n",
		"@a:registry=https://u:p@a.test\n@b:registry=https://b.test\n//a.test/:_password=x\n",
		"registry=https://npm.acme.test\n//npm.acme.test/:_authToken=${NPM_TOKEN}\n",
	}
	for _, in := range inputs {
		out := string(ScrubNPMRC([]byte(in)).Content)
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line == "" {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				t.Errorf("projected line is not key=value: %q (input %q)", line, in)
				continue
			}
			if credentialKey.MatchString(key) {
				t.Errorf("projected a credential-bearing key %q (input %q)", key, in)
			}
			if !isRegistryMapping(key) {
				t.Errorf("projected a non-mapping key %q (input %q)", key, in)
			}
			u, err := url.Parse(value)
			if err != nil {
				t.Errorf("projected an unparseable URL %q (input %q)", value, in)
				continue
			}
			if u.User != nil {
				t.Errorf("projected inline userinfo in %q (input %q)", value, in)
			}
		}
		for _, marker := range []string{"LIVETOKEN", "hunter2", "aGVsbG86d29ybGQ=", "NPM_TOKEN", "u:p"} {
			if strings.Contains(out, marker) {
				t.Errorf("scrub leaked %q\n input: %q\noutput: %q", marker, in, out)
			}
		}
	}
}

func TestProjectNPMWritesScrubbedFile(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	npmrc := filepath.Join(home, ".npmrc")
	body := "@acme:registry=https://npm.acme.test\n//npm.acme.test/:_authToken=SECRET\n"
	if err := os.WriteFile(npmrc, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	projs, err := Project([]string{sandboxprofile.RegistryConfigNPM}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(projs) != 1 {
		t.Fatalf("got %d projection(s), want 1", len(projs))
	}
	p := projs[0]
	if p.EnvVar != "NPM_CONFIG_USERCONFIG" {
		t.Errorf("EnvVar = %q", p.EnvVar)
	}
	if p.Source != npmrc {
		t.Errorf("Source = %q, want %q", p.Source, npmrc)
	}
	got, err := os.ReadFile(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "@acme:registry=https://npm.acme.test\n"; string(got) != want {
		t.Errorf("projected file = %q, want %q", got, want)
	}
	if strings.Contains(string(got), "SECRET") {
		t.Fatal("projected file leaked the auth token")
	}
	info, err := os.Stat(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("projection mode = %o, want 600", perm)
	}
	// The summary goes into the launch log; it must not echo the file body.
	if strings.Contains(p.Summary(), "SECRET") {
		t.Errorf("summary leaked secret material: %q", p.Summary())
	}
}

func TestProjectNPMAbsentOrMappinglessIsNoop(t *testing.T) {
	t.Run("missing npmrc", func(t *testing.T) {
		setHome(t, t.TempDir())
		projs, err := Project([]string{sandboxprofile.RegistryConfigNPM}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if len(projs) != 0 {
			t.Fatalf("got %d projection(s), want none", len(projs))
		}
	})

	t.Run("npmrc with no mapping", func(t *testing.T) {
		home := t.TempDir()
		setHome(t, home)
		if err := os.WriteFile(filepath.Join(home, ".npmrc"), []byte("_auth=x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		projs, err := Project([]string{sandboxprofile.RegistryConfigNPM}, dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(projs) != 0 {
			t.Fatalf("got %d projection(s), want none", len(projs))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("wrote %d file(s) for a mappingless npmrc, want none", len(entries))
		}
	})
}

func TestProjectUnknownEcosystem(t *testing.T) {
	if _, err := Project([]string{"nope"}, t.TempDir()); err == nil {
		t.Fatal("want error for unknown ecosystem")
	}
}

// --- review findings on the registry_config projection ---

// TestScrubNPMRCRefusesQueryStringSecret is the leak the review found: a
// secret outside the userinfo (?apiKey=…) was projected verbatim, despite
// the package's "no credential can survive" invariant.
func TestScrubNPMRCRefusesQueryStringSecret(t *testing.T) {
	for _, src := range []string{
		"@acme:registry=https://npm.acme.test/api/npm/?apiKey=SUPERSECRET\n",
		"@acme:registry=https://user:pw@npm.acme.test/api/?tok=SUPERSECRET\n",
		"@acme:registry=https://npm.acme.test/api#SUPERSECRET\n",
	} {
		got := ScrubNPMRC([]byte(src))
		if strings.Contains(string(got.Content), "SUPERSECRET") {
			t.Errorf("projected a secret from the URL: %q (input %q)", got.Content, src)
		}
		if len(got.Rejected) != 1 {
			t.Errorf("rejected = %+v, want 1 entry explaining the refusal (input %q)", got.Rejected, src)
		}
	}
}

// TestScrubNPMRCHonorsNpmValueSyntax covers mappings npm acts on but Go's
// url.Parse rejects verbatim. Silently skipping these left the user with the
// exact unexplained 404 this feature exists to prevent.
func TestScrubNPMRCHonorsNpmValueSyntax(t *testing.T) {
	t.Run("quoted value", func(t *testing.T) {
		got := ScrubNPMRC([]byte("@acme:registry=\"https://npm.acme.test\"\n"))
		if want := "@acme:registry=https://npm.acme.test\n"; string(got.Content) != want {
			t.Errorf("content = %q, want %q", got.Content, want)
		}
	})
	t.Run("env var interpolation", func(t *testing.T) {
		t.Setenv("ART_HOST", "npm.acme.test")
		got := ScrubNPMRC([]byte("@acme:registry=https://${ART_HOST}/api/npm/npm/\n"))
		if want := "@acme:registry=https://npm.acme.test/api/npm/npm/\n"; string(got.Content) != want {
			t.Errorf("content = %q, want %q", got.Content, want)
		}
	})
	t.Run("unset env var is reported, not silently skipped", func(t *testing.T) {
		got := ScrubNPMRC([]byte("@acme:registry=https://${OMAC_TEST_UNSET_HOST}/api/\n"))
		if len(got.KeptKeys) != 0 {
			t.Errorf("kept %v for an unresolvable value", got.KeptKeys)
		}
		if len(got.Rejected) != 1 {
			t.Errorf("rejected = %+v, want 1 entry", got.Rejected)
		}
	})
}

// TestScrubNPMRCRefusesUnauthenticatedGlobalRegistry is the regression the
// review identified: projecting a global `registry` whose token was dropped
// points npm at a private mirror it cannot authenticate to, breaking even
// the public installs that work today with the file fully masked.
func TestScrubNPMRCRefusesUnauthenticatedGlobalRegistry(t *testing.T) {
	got := ScrubNPMRC([]byte("registry=https://npm.acme.test\n//npm.acme.test/:_authToken=T\n"))
	if len(got.KeptKeys) != 0 {
		t.Errorf("projected %v; the global registry needs auth omac cannot supply", got.KeptKeys)
	}
	if len(got.Rejected) != 1 || !strings.Contains(got.Rejected[0].Reason, "authentication") {
		t.Fatalf("rejected = %+v, want one entry explaining the auth problem", got.Rejected)
	}

	// A *scoped* mapping to the same host is kept: that scope was already
	// failing, so there is no regression — but it must be flagged.
	scoped := ScrubNPMRC([]byte("@acme:registry=https://npm.acme.test\n//npm.acme.test/:_authToken=T\n"))
	if len(scoped.KeptKeys) != 1 {
		t.Errorf("kept = %v, want the scoped mapping", scoped.KeptKeys)
	}
	if len(scoped.NeedsAuth) != 1 {
		t.Errorf("NeedsAuth = %v, want the scoped mapping flagged", scoped.NeedsAuth)
	}

	// Without a credential entry the global mapping is fine to project.
	plain := ScrubNPMRC([]byte("registry=https://npm.acme.test\n"))
	if len(plain.KeptKeys) != 1 || len(plain.NeedsAuth) != 0 {
		t.Errorf("kept = %v, needsAuth = %v; want the mapping projected cleanly", plain.KeptKeys, plain.NeedsAuth)
	}
}

// TestProjectNPMUnreadableConfigIsNotFatal keeps an opt-in convenience from
// taking the whole launch down.
func TestProjectNPMUnreadableConfigIsNotFatal(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	// A directory where the file is expected makes the read fail with
	// something other than IsNotExist.
	if err := os.Mkdir(filepath.Join(home, ".npmrc"), 0o755); err != nil {
		t.Fatal(err)
	}
	projs, err := Project([]string{sandboxprofile.RegistryConfigNPM}, t.TempDir())
	if err != nil {
		t.Fatalf("read failure must not be fatal, got: %v", err)
	}
	if len(projs) != 1 || projs[0].Projected() {
		t.Fatalf("projections = %+v, want one non-projected entry", projs)
	}
	if projs[0].Warning == "" {
		t.Error("no warning explaining why nothing was projected")
	}
}

// TestInspectNPMReportsRejections keeps doctor from going silent on config
// it cannot use.
func TestInspectNPMReportsRejections(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	body := "@acme:registry=https://npm.acme.test/api/?apiKey=SECRET\n"
	if err := os.WriteFile(filepath.Join(home, ".npmrc"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	notice, err := InspectNPM(false, false)
	if err != nil {
		t.Fatal(err)
	}
	if notice == nil {
		t.Fatal("no notice for an npmrc whose mapping cannot be projected")
	}
	if len(notice.Rejected) != 1 {
		t.Errorf("notice.Rejected = %+v, want 1 entry", notice.Rejected)
	}
}
