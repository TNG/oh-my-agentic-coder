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
//   - FsAllowLabels: paths the agent CAN read/write (workdir, cache
//     dir, $TMPDIR) — the positive counterpart to FsDenyPaths.
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

	// FsAllowLabels are the fs_allow probe labels (audit.sh) for paths a
	// LEGITIMATE user needs — workdir read/write, the harness cache dir,
	// $TMPDIR — that must stay accessible. The positive counterpart to
	// FsDenyPaths: catches a hardening change (a new ProtectedPaths
	// entry, a tightened deny-glob) that accidentally shadows something
	// ordinary work depends on, which FsDenyPaths alone cannot detect.
	FsAllowLabels []string

	// FsWriteDenyPaths are system paths that must NOT be writable.
	// The sandbox grants them read-only; write attempts must fail.
	FsWriteDenyPaths []string

	// FsExecProbePaths are binaries on read-only mounts. Whether exec
	// is denied depends on the backend — we probe to document behavior,
	// not assert a specific outcome. The test logs the result.
	FsExecProbePaths []string

	// NetDenyDomain is a domain the agent must NOT be able to reach.
	// The test prompts the agent to curl it and asserts failure.
	NetDenyDomain string

	// SidecarReachable: if true, the test asserts the agent can call
	// the sidecar's /whoami endpoint and see the secret fingerprint.
	SidecarReachable bool

	// CrossSkillIsolated: if true, the test asserts the agent CANNOT
	// reach another registered skill's sidecar (e.g. echo-rest from
	// self-audit). Each skill's sidecar should be isolated.
	CrossSkillIsolated bool

	// SymlinkEscapeDenyPaths are denied paths the agent targets via a
	// symlink placed inside the allowed workdir, to check whether the
	// sandbox enforces the resolved (real) path rather than only the
	// literal path the agent opened. The test asserts denial through the
	// symlink indirection just as it does for a direct path.
	SymlinkEscapeDenyPaths []string
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
			"~/.aws/credentials",
			"~/.kube/config",
			"~/.docker/config.json",
			"~/.netrc",
			"~/.gnupg/pubring.gpg",
			"~/.password-store",
			"~/.local/share/keyrings",
			"~/.bash_history",
			"~/.zshrc",
			"~/.profile",
			"~/.env",
			"~/.envrc",
			"/root/.bashrc", // Linux; macOS baseline doesn't include /root
			"~/.azure/credentials",
			"~/.config/gcloud/credentials.db",
			"/var/run/docker.sock", // container-escape vector if reachable
		},
		// FsAllowLabels mirror the labels audit.sh's fs_allow probe emits
		// for a legitimate user's paths. Positive counterpart to
		// FsDenyPaths: catches a hardening change that accidentally
		// shadows the workdir, cache dir, or $TMPDIR.
		FsAllowLabels: []string{
			"--- write workdir file ---",
			"--- read workdir file ---",
			"--- write $HOME/.cache file ---",
			"--- write ${TMPDIR:-/tmp} file ---",
		},
		// FsWriteDenyPaths are system paths that must NOT be writable.
		// The sandbox grants them read-only; write attempts must fail.
		FsWriteDenyPaths: []string{
			"/etc/omac-audit-test",
			"/usr/omac-audit-test",
			"/bin/omac-audit-test",
			"/sbin/omac-audit-test",
		},
		// FsExecDenyPaths are binaries on read-only mounts. Whether the
		// sandbox denies exec on read-only paths depends on the backend
		// (bwrap mounts /usr read-only but exec is typically allowed).
		// We probe this to DOCUMENT the current behavior, not to assert
		// a specific outcome — exec on read-only mounts is a platform
		// decision, not a contract. The test logs the result.
		FsExecProbePaths: []string{
			"/usr/bin/python3",
			"/bin/sh",
		},
		NetDenyDomain:      "blocked.example.com",
		SidecarReachable:   true,
		CrossSkillIsolated: true, // echo-rest sidecar must NOT be reachable from self-audit
		// Mirrors the symlink targets hardcoded in audit.sh's "symlink"
		// probe (~/.ssh/id_rsa for read, /etc/omac-audit-test for write).
		SymlinkEscapeDenyPaths: []string{
			"~/.ssh/id_rsa",
			"/etc/omac-audit-test",
		},
	}
}
