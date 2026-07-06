package profileaudit

import "github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"

// BaselineSecretPaths returns the paths omac already denies by default
// (PlatformBaseline().ProtectedPaths). Exported so the check can
// distinguish "profile weakens a baseline protection" (category E) from
// "profile exposes a path omac never protected" (category A).
func BaselineSecretPaths() []string {
	return sandboxprofile.PlatformBaseline().ProtectedPaths
}

// ExtensionSecretPaths are known secret-bearing paths NOT in the baseline.
// Curated, small list. Add new entries here only — each entry must be a
// path that genuinely holds a credential and is not already covered by
// BaselineSecretPaths() (a guard test enforces no overlap).
var ExtensionSecretPaths = []string{
	"~/.pypirc",                // PyPI upload token
	"~/.config/github-copilot", // Copilot OAuth token
	"~/.config/gh",             // GitHub CLI token
	"~/.gitconfig",             // may embed tokens (insteadOf/url cred)
	"~/.config/hub",            // legacy GitHub hub token
	"~/.cf",                    // Cloud Foundry CLI
}

// SecretBasenameGlobs are filename patterns matched against grants that
// use wildcards or broad directory grants (e.g. allow: ["."]). Each
// entry must be a valid filepath.Match pattern (a guard test enforces
// this).
var SecretBasenameGlobs = []string{
	".env",
	"*.env",
	"*.key",
	"*.pem",
	"*token*",
	"*secret*",
	"id_rsa*",
	"*.pfx",
	"*.p12",
}
