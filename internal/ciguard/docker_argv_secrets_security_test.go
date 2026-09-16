package ciguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecurityDockerEnvFlagsKeepSecretValuesOffArgv asserts that
// e2e-docker.sh's env_flags() does not embed a secret's VALUE in the
// docker CLI's own argv — only the name, with the value passed some other
// way (an env-file, stdin, or the parent's own environment via `-e NAME`
// with no `=value`).
//
// `docker run -e SKAINET_TOKEN=<value>` puts the token in the docker
// client PROCESS's own command line, readable by any local user via
// /proc/<pid>/cmdline or `ps auxww` for as long as the client runs —
// internal/supervisor's own doc comment states the opposite rule for
// omac's sidecar spawning ("Secrets are passed via env only; they never
// appear on argv").
//
// Extracted from the script rather than sourcing it whole: e2e-docker.sh
// unconditionally calls main "$@" at file scope, which would run its CLI
// dispatch (and exit the test process) if sourced directly.
func TestSecurityDockerEnvFlagsKeepSecretValuesOffArgv(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts", "e2e-docker.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("control: %s not found: the fixture is broken, not the security property: %v", script, err)
	}

	extract := exec.Command("awk", `/^env_flags\(\) \{/,/^}/`, script)
	fnBody, err := extract.Output()
	if err != nil {
		t.Fatalf("extract env_flags(): %v", err)
	}
	if len(fnBody) == 0 {
		t.Fatalf("control: env_flags() was not found in %s: the fixture is broken, not the security property", script)
	}

	driver := string(fnBody) + "\nenv_flags\n"
	cmd := exec.Command("bash", "-c", driver)
	cmd.Env = append(os.Environ(), "SKAINET_TOKEN=hunter2-secret-value")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run env_flags(): %v", err)
	}
	flags := string(out)

	// Control: the flag for the token IS emitted (the fixture reaches the
	// vulnerable code), so a fix that drops the flag entirely wouldn't
	// make this test pass for the wrong reason.
	if !strings.Contains(flags, "SKAINET_TOKEN") {
		t.Fatalf("control: no SKAINET_TOKEN flag was emitted at all: the fixture is broken, not the security property\n%s", flags)
	}

	if strings.Contains(flags, "hunter2-secret-value") {
		t.Errorf("env_flags() embedded the secret's VALUE in a docker argv element, visible to any local user via /proc/<pid>/cmdline for as long as the docker client runs: %q", flags)
	}
}
