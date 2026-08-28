package sandboxrun

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// writeNpmrc points HOME at a temp dir holding the given npmrc body.
func writeNpmrc(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".npmrc")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetupRegistryConfigNoopWhenUnset(t *testing.T) {
	writeNpmrc(t, "@acme:registry=https://npm.acme.test\n")
	grants := &Grants{}
	injected := map[string]string{}
	var stderr bytes.Buffer

	cleanup, err := setupRegistryConfig(&sandboxprofile.Profile{}, grants, injected, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if len(grants.ReadPaths) != 0 {
		t.Errorf("granted %v with registry_config unset", grants.ReadPaths)
	}
	if len(injected) != 0 {
		t.Errorf("injected %v with registry_config unset", injected)
	}
	if stderr.Len() != 0 {
		t.Errorf("unexpected output: %q", stderr.String())
	}
}

func TestSetupRegistryConfigGrantsProjectionOnly(t *testing.T) {
	npmrc := writeNpmrc(t, "@acme:registry=https://npm.acme.test\n//npm.acme.test/:_authToken=SECRET\n")
	grants := &Grants{}
	injected := map[string]string{}
	var stderr bytes.Buffer

	profile := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{RegistryConfig: []string{sandboxprofile.RegistryConfigNPM}},
	}
	cleanup, err := setupRegistryConfig(profile, grants, injected, &stderr)
	if err != nil {
		t.Fatal(err)
	}

	projected := injected["NPM_CONFIG_USERCONFIG"]
	if projected == "" {
		t.Fatal("NPM_CONFIG_USERCONFIG was not injected")
	}
	if !slices.Contains(grants.ReadPaths, projected) {
		t.Errorf("ReadPaths %v does not include the projection %q", grants.ReadPaths, projected)
	}
	if slices.Contains(grants.ReadPaths, npmrc) {
		t.Error("granted the protected host npmrc; only the projection may be granted")
	}
	if len(grants.ReadPaths) != 1 {
		t.Errorf("ReadPaths = %v, want exactly the projection", grants.ReadPaths)
	}
	body, err := os.ReadFile(projected)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "SECRET") {
		t.Fatalf("projection leaked the token: %q", body)
	}
	if !strings.Contains(stderr.String(), "registry_config") {
		t.Errorf("launch log did not mention the projection: %q", stderr.String())
	}

	// The projection must not outlive the run.
	cleanup()
	if _, err := os.Stat(projected); !os.IsNotExist(err) {
		t.Errorf("projection survived cleanup: %v", err)
	}
}

func TestSetupRegistryConfigWarnsWhenOverrideDenyAlsoGrantsHostFile(t *testing.T) {
	npmrc := writeNpmrc(t, "@acme:registry=https://npm.acme.test\n")
	grants := &Grants{}
	var stderr bytes.Buffer

	profile := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{
			RegistryConfig: []string{sandboxprofile.RegistryConfigNPM},
			OverrideDeny:   []string{npmrc},
		},
	}
	cleanup, err := setupRegistryConfig(profile, grants, map[string]string{}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	out := stderr.String()
	if !strings.Contains(out, "override_deny") || !strings.Contains(out, "WARNING") {
		t.Errorf("expected a warning that override_deny exposes the real file, got: %q", out)
	}
}

func TestSetupRegistryConfigReportsNothingToProject(t *testing.T) {
	writeNpmrc(t, "_auth=SECRET\n") // credentials only, no mapping
	grants := &Grants{}
	injected := map[string]string{}
	var stderr bytes.Buffer

	profile := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{RegistryConfig: []string{sandboxprofile.RegistryConfigNPM}},
	}
	cleanup, err := setupRegistryConfig(profile, grants, injected, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if len(grants.ReadPaths) != 0 || len(injected) != 0 {
		t.Errorf("projected nothing but still granted %v / injected %v", grants.ReadPaths, injected)
	}
	if !strings.Contains(stderr.String(), "no registry mapping") {
		t.Errorf("silent no-op; want an explicit notice, got: %q", stderr.String())
	}
}

func TestSetupRegistryConfigMissingHostFileIsQuietNoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no .npmrc at all
	grants := &Grants{}
	injected := map[string]string{}
	var stderr bytes.Buffer

	profile := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{RegistryConfig: []string{sandboxprofile.RegistryConfigNPM}},
	}
	cleanup, err := setupRegistryConfig(profile, grants, injected, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if len(grants.ReadPaths) != 0 || len(injected) != 0 {
		t.Errorf("granted %v / injected %v for a missing npmrc", grants.ReadPaths, injected)
	}
}

// TestSetupRegistryConfigWarnsWhenEnvVarAlreadySet covers the review finding
// that an injected var silently wins over a value the user forwarded
// themselves, dropping their own config with no notice.
func TestSetupRegistryConfigWarnsWhenEnvVarAlreadySet(t *testing.T) {
	writeNpmrc(t, "@acme:registry=https://npm.acme.test\n")
	t.Setenv("NPM_CONFIG_USERCONFIG", "/home/dev/custom-npmrc")

	grants := &Grants{}
	injected := map[string]string{}
	var stderr bytes.Buffer
	profile := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{RegistryConfig: []string{sandboxprofile.RegistryConfigNPM}},
	}
	cleanup, err := setupRegistryConfig(profile, grants, injected, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	out := stderr.String()
	if !strings.Contains(out, "already set") || !strings.Contains(out, "/home/dev/custom-npmrc") {
		t.Errorf("no warning naming the overridden path; got:\n%s", out)
	}
	// The projection still wins — the warning explains, it does not defer.
	if injected["NPM_CONFIG_USERCONFIG"] == "/home/dev/custom-npmrc" {
		t.Error("projection did not take precedence")
	}
}

// TestSetupRegistryConfigQuietWhenEnvVarUnset keeps the warning from firing on
// the ordinary path.
func TestSetupRegistryConfigQuietWhenEnvVarUnset(t *testing.T) {
	writeNpmrc(t, "@acme:registry=https://npm.acme.test\n")
	t.Setenv("NPM_CONFIG_USERCONFIG", "")

	var stderr bytes.Buffer
	profile := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{RegistryConfig: []string{sandboxprofile.RegistryConfigNPM}},
	}
	cleanup, err := setupRegistryConfig(profile, &Grants{}, map[string]string{}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if strings.Contains(stderr.String(), "already set") {
		t.Errorf("spurious override warning; got:\n%s", stderr.String())
	}
}
