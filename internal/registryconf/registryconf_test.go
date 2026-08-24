package registryconf

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
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
			name:     "non-URL mapping value is dropped",
			src:      "registry=not-a-url\n",
			wantKeys: nil,
			dropped:  1,
		},
		{
			name:     "non-http scheme is dropped",
			src:      "registry=file:///tmp/evil\n",
			wantKeys: nil,
			dropped:  1,
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
	t.Setenv("HOME", home)
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
		t.Setenv("HOME", t.TempDir())
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
		t.Setenv("HOME", home)
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
