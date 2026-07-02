//go:build e2e

package e2e

// AllowanceSpec is the human-readable security contract for the e2e
// sandbox. It declares exactly what the agent is allowed to see and do,
// and what it must NOT be able to do. Both the sandbox profile writer
// and the security assertions derive from this struct, so the spec is
// the single source of truth — a developer reads it to understand the
// security boundary, and the test enforces it.
//
// Categories:
//
//   - EnvAllowVars: env vars the agent IS allowed to see.
//     Everything else is stripped by FilterEnv. SKAINET_TOKEN is
//     included for harnesses that need it (codex, copilot, opencode);
//     claude-code uses ANTHROPIC_AUTH_TOKEN instead.
//   - EnvDenyVars: env vars the agent must NOT see. These are
//     asserted as negative — if they appear in the agent output,
//     the sandbox leaked them.
//   - EnvExpectVisible: env vars the agent SHOULD see (positive
//     assertion — verifies the sandbox passes them through).
//   - FsDenyPaths: paths the agent must NOT be able to read.
//   - FsAllowPaths: paths the agent CAN read/write (echo-rest
//     workdir, skill mounts, cache dirs).
//   - NetAllowDomains: domains the agent CAN reach.
//   - NetDenyDomain: a domain the agent must NOT be able to reach.
//   - SidecarReachable: the sidecar /whoami endpoint should work.
type AllowanceSpec struct {
	// EnvAllowVars is written to the profile's environment.allow_vars.
	// Only these env-var names (or prefixes) pass into the sandbox.
	// Empty means "allow all" — do not use in security tests.
	EnvAllowVars []string

	// EnvDenyVars are env vars that must NOT appear in the agent's
	// env output. These are the negative assertions.
	EnvDenyVars []string

	// EnvExpectVisible are env vars that SHOULD appear in the agent's
	// env output. These are the positive assertions.
	EnvExpectVisible []string

	// FsDenyPaths are filesystem paths the agent must NOT be able to
	// read. The test prompts the agent to cat each and asserts denial.
	FsDenyPaths []string

	// NetDenyDomain is a domain the agent must NOT be able to reach.
	// The test prompts the agent to curl it and asserts failure.
	NetDenyDomain string

	// SidecarReachable: if true, the test asserts the agent can call
	// the sidecar's /whoami endpoint and see the secret fingerprint.
	SidecarReachable bool
}

// allowanceSpecFor returns the allowance spec for a harness.
// The spec is deliberately per-harness because each harness has
// different env-var requirements (claude-code uses ANTHROPIC_AUTH_TOKEN,
// others use SKAINET_TOKEN directly).
func allowanceSpecFor(h harnessConfig) AllowanceSpec {
	// Base env vars all harnesses need.
	allow := []string{
		"OMAC_*",
		"HOME",
		"PATH",
		"PWD",
		"TMPDIR",
		"LANG",
		"LC_*",
		"TERM",
		"SHELL",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"XDG_STATE_HOME",
		"NPM_CONFIG_PREFIX",
	}

	// Add harness-specific env vars the agent needs.
	allow = append(allow, h.EnvVarsForAllow()...)

	// AUDIT_SECRET must NEVER be visible to the agent — it's a sidecar
	// secret delivered via env_passthrough. The sandbox must strip it.
	deny := []string{
		"AUDIT_SECRET",
	}

	// For claude-code, SKAINET_TOKEN is not needed (uses ANTHROPIC_AUTH_TOKEN).
	// For codex/copilot/opencode, SKAINET_TOKEN IS needed.
	if h.Name == "claude-code" {
		deny = append(deny, "SKAINET_TOKEN")
	}

	return AllowanceSpec{
		EnvAllowVars:     allow,
		EnvDenyVars:      deny,
		EnvExpectVisible: h.ExpectVisibleEnv(),
		FsDenyPaths: []string{
			"/etc/shadow",
			"~/.ssh/id_rsa",
			"/root/.bashrc", // Linux; macOS baseline doesn't include /root
		},
		NetDenyDomain:    "blocked.example.com",
		SidecarReachable: true,
	}
}
