package sandboxrun

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/registryconf"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// setupRegistryConfig honors filesystem.registry_config: it derives a
// credential-free copy of each requested package-manager config, grants
// read access to that copy alone, and points the tool at it through its
// native config env var. The protected host file stays masked.
//
// grants and injected are mutated in place, so this must run after grants
// are resolved and before BuildChildArgv turns them into backend rules.
// The returned cleanup removes the projection directory; it is safe to
// call even on the error paths.
func setupRegistryConfig(merged *sandboxprofile.Profile, grants *Grants, injected map[string]string, stderr io.Writer) (func(), error) {
	noop := func() {}
	ecosystems := merged.Filesystem.RegistryConfig
	if len(ecosystems) == 0 {
		return noop, nil
	}

	dir, err := os.MkdirTemp("", "omac-registryconf-")
	if err != nil {
		return noop, fmt.Errorf("registry_config: create projection dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return noop, fmt.Errorf("registry_config: secure projection dir: %w", err)
	}

	projections, err := registryconf.Project(ecosystems, dir)
	if err != nil {
		cleanup()
		return noop, err
	}
	overrides := sandboxprofile.BuildOverrideLookup(merged.Filesystem.OverrideDeny)
	granted := 0
	for _, p := range projections {
		// Every rejection is reported: a mapping that does not reach the
		// sandbox produces exactly the silent 404 this feature exists to
		// prevent, so it must never be dropped quietly.
		for _, r := range p.Rejected {
			fmt.Fprintf(stderr, "omac sandbox: WARNING: registry_config %s: not projecting %q — %s\n",
				p.Ecosystem, r.Key, r.Reason)
		}
		if p.Warning != "" {
			fmt.Fprintf(stderr, "omac sandbox: WARNING: registry_config %s: %s\n", p.Ecosystem, p.Warning)
		}
		if !p.Projected() {
			continue
		}

		// Grant exactly the projected file, read-only. The host file is
		// untouched and stays protected.
		grants.ReadPaths = append(grants.ReadPaths, p.Path)
		injected[p.EnvVar] = p.Path
		granted++
		fmt.Fprintf(stderr, "omac sandbox: registry_config: %s\n", p.Summary())
		if len(p.NeedsAuth) > 0 {
			fmt.Fprintf(stderr, "omac sandbox: WARNING: registry_config %s: %s point at a registry that needs authentication, "+
				"and the credential is deliberately not copied into the sandbox — installs from it may fail with 401/403. "+
				"Supply the token to the registry another way, or expect those packages to be unavailable.\n",
				p.Ecosystem, strings.Join(p.NeedsAuth, ", "))
		}
		if overrides[p.Source] {
			fmt.Fprintf(stderr, "omac sandbox: WARNING: %s is also in filesystem.override_deny, so the sandbox can read the "+
				"real file including any auth token it holds. The projection makes that grant unnecessary — "+
				"drop the override_deny entry to keep the credential protected.\n", p.Source)
		}
	}
	if granted == 0 {
		// The user asked for a projection and got none; silence here reads
		// as "it worked".
		fmt.Fprintf(stderr, "omac sandbox: registry_config (%s): no registry mapping was projected; "+
			"scoped packages will resolve against the default registry\n", strings.Join(ecosystems, ", "))
		cleanup()
		return noop, nil
	}
	return cleanup, nil
}
