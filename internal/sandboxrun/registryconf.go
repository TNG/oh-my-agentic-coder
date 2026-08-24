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
	if len(projections) == 0 {
		// Nothing to project (no config file, or no registry mapping in
		// it). Say so: the user asked for a projection and got none, and
		// silence here reads as "it worked".
		fmt.Fprintf(stderr, "omac sandbox: registry_config (%s): no registry mapping found to project; "+
			"scoped packages will resolve against the default registry\n", strings.Join(ecosystems, ", "))
		cleanup()
		return noop, nil
	}

	overrides := sandboxprofile.BuildOverrideLookup(merged.Filesystem.OverrideDeny)
	for _, p := range projections {
		// Grant exactly the projected file, read-only. The host file is
		// untouched and stays protected.
		grants.ReadPaths = append(grants.ReadPaths, p.Path)
		injected[p.EnvVar] = p.Path
		fmt.Fprintf(stderr, "omac sandbox: registry_config: %s\n", p.Summary())
		if overrides[p.Source] {
			fmt.Fprintf(stderr, "omac sandbox: WARNING: %s is also in filesystem.override_deny, so the sandbox can read the "+
				"real file including any auth token it holds. The projection makes that grant unnecessary — "+
				"drop the override_deny entry to keep the credential protected.\n", p.Source)
		}
	}
	return cleanup, nil
}
