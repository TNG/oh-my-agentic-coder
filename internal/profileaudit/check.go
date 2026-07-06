package profileaudit

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// Check statically lints a resolved sandbox profile against known secret
// locations and network attack vectors. It performs no filesystem or
// network I/O. The returned findings are sorted by severity
// (high → medium → low), then by category, then by field.
func Check(profile *sandboxprofile.Profile) []Finding {
	var findings []Finding
	findings = append(findings, checkOverrideDeny(profile)...)
	findings = append(findings, checkFSGrants(profile)...)
	findings = append(findings, checkNetwork(profile)...)
	sortFindings(findings)
	return findings
}

// sortFindings orders findings by severity (high first), then category,
// then field, then value. Stable so equal-keyed entries keep insertion
// order.
func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		ri, rj := severityRank(findings[i].Severity), severityRank(findings[j].Severity)
		if ri != rj {
			return ri < rj
		}
		if findings[i].Category != findings[j].Category {
			return findings[i].Category < findings[j].Category
		}
		if findings[i].Field != findings[j].Field {
			return findings[i].Field < findings[j].Field
		}
		return findings[i].Value < findings[j].Value
	})
}

// checkOverrideDeny flags every override_deny entry that removes a
// baseline-protected path. Each such entry is a deliberate weakening of
// a credential protection and is always HIGH.
//
// ponytail: baseline paths are stored unexpanded (~/.ssh); we expand
// both sides before comparing so tilde/env-var entries match.
func checkOverrideDeny(profile *sandboxprofile.Profile) []Finding {
	if len(profile.Filesystem.OverrideDeny) == 0 {
		return nil
	}
	// BaselineSecretPaths returns unexpanded paths (e.g. ~/.ssh).
	// Expand each so the comparison is in canonical absolute form.
	// Entries that fail to expand are kept verbatim, mirroring
	// EffectiveProtectedPaths (baseline.go:160).
	baseSet := make(map[string]bool)
	for _, p := range BaselineSecretPaths() {
		exp, err := sandboxprofile.ExpandPath(p)
		if err != nil {
			baseSet[p] = true
			continue
		}
		baseSet[exp] = true
	}
	var findings []Finding
	for _, entry := range profile.Filesystem.OverrideDeny {
		// override_deny entries may use ~ or $VAR. Expand for comparison.
		// On expansion failure, compare the verbatim entry against
		// baseSet before skipping — matches EffectiveProtectedPaths.
		exp, err := sandboxprofile.ExpandPath(entry)
		if err != nil {
			if baseSet[entry] {
				findings = append(findings, Finding{
					Severity: SeverityHigh,
					Category: CatOverrideDeny,
					Field:    "filesystem.override_deny",
					Value:    entry,
					Message:  "removes baseline protection on " + entry + " (" + secretDescription(entry) + ")",
				})
			}
			continue
		}
		if baseSet[exp] {
			findings = append(findings, Finding{
				Severity: SeverityHigh,
				Category: CatOverrideDeny,
				Field:    "filesystem.override_deny",
				Value:    entry,
				Message:  "removes baseline protection on " + exp + " (" + secretDescription(exp) + ")",
			})
		}
	}
	return findings
}

// secretDescription returns a short human-readable hint for a known
// secret path, used in finding messages.
func secretDescription(path string) string {
	switch {
	case strings.Contains(path, ".ssh"):
		return "SSH private keys"
	case strings.Contains(path, ".aws"):
		return "AWS credentials"
	case strings.Contains(path, ".azure"):
		return "Azure CLI credentials"
	case strings.Contains(path, ".gcloud"), strings.Contains(path, "gcloud"):
		return "GCP credentials"
	case strings.Contains(path, ".kube"):
		return "Kubernetes config"
	case strings.Contains(path, ".docker"):
		return "Docker registry tokens"
	case strings.Contains(path, ".gnupg"):
		return "GPG keys"
	case strings.Contains(path, ".netrc"):
		return "HTTP credentials"
	case strings.Contains(path, ".npmrc"):
		return "npm token"
	case strings.Contains(path, ".vault-token"):
		return "Vault token"
	case strings.Contains(path, "Keychain"), strings.Contains(path, "keyring"):
		return "OS keychain/keyring"
	case strings.Contains(path, ".pypirc"):
		return "PyPI upload token"
	case strings.Contains(path, "github-copilot"):
		return "Copilot OAuth token"
	case strings.Contains(path, ".config/gh"):
		return "GitHub CLI token"
	case strings.Contains(path, ".gitconfig"):
		return "git config (may embed tokens)"
	case strings.Contains(path, ".config/hub"):
		return "GitHub hub token"
	case strings.Contains(path, ".cf"):
		return "Cloud Foundry CLI"
	default:
		return "credentials"
	}
}

// checkFSGrants flags filesystem grants (allow/read/write/allow_unix_dir)
// that expose known secret paths or could match known secret basenames.
func checkFSGrants(profile *sandboxprofile.Profile) []Finding {
	type slot struct {
		field   string
		entries []string
	}
	slots := []slot{
		{"filesystem.allow", profile.Filesystem.Allow},
		{"filesystem.read", profile.Filesystem.Read},
		{"filesystem.write", profile.Filesystem.Write},
		{"filesystem.allow_unix_dir", profile.Filesystem.AllowUnixDir},
	}
	base := BaselineSecretPaths()
	ext := ExtensionSecretPaths
	var findings []Finding
	for _, s := range slots {
		for _, entry := range s.entries {
			findings = append(findings, checkOneFSGrant(s.field, entry, base, ext)...)
		}
	}
	return findings
}

// checkOneFSGrant inspects a single grant entry.
func checkOneFSGrant(field, entry string, baseline, extension []string) []Finding {
	// Literal broad-glob grants (".", "*", "./", ".") cannot be resolved
	// to an explicit path and could expose any file. Treat them as broad
	// grants regardless of ExpandPath succeeding on ".".
	if isBroadGlob(entry) {
		return checkBroadGrant(field, entry)
	}
	exp, err := sandboxprofile.ExpandPath(entry)
	if err == nil {
		return checkExplicitGrant(field, entry, exp, baseline, extension)
	}
	return checkBroadGrant(field, entry)
}

// isBroadGlob reports whether entry is a literal broad/wildcard grant
// that cannot be meaningfully compared against secret paths.
func isBroadGlob(entry string) bool {
	switch entry {
	case ".", "*", "./", "./*", "**":
		return true
	}
	return false
}

// checkExplicitGrant compares an expanded grant path against the known
// secret path lists. A grant that equals or is a parent of a secret
// path is flagged. A subpath of a secret path is not (it doesn't
// expose the secret itself).
func checkExplicitGrant(field, entry, exp string, baseline, extension []string) []Finding {
	var findings []Finding
	for _, sp := range baseline {
		expandedSP, err := sandboxprofile.ExpandPath(sp)
		if err != nil {
			continue
		}
		if exp == expandedSP || isParent(exp, expandedSP) {
			findings = append(findings, Finding{
				Severity: SeverityHigh,
				Category: CatFSGrant,
				Field:    field,
				Value:    entry,
				Message:  "intersects baseline protected path " + expandedSP + " (" + secretDescription(expandedSP) + ")",
			})
			return findings
		}
	}
	for _, sp := range extension {
		expandedSP, err := sandboxprofile.ExpandPath(sp)
		if err != nil {
			continue
		}
		if exp == expandedSP || isParent(exp, expandedSP) {
			findings = append(findings, Finding{
				Severity: SeverityMedium,
				Category: CatFSGrant,
				Field:    field,
				Value:    entry,
				Message:  "overlaps known secret path " + expandedSP + " not in baseline (" + secretDescription(expandedSP) + ")",
			})
			return findings
		}
	}
	return findings
}

// checkBroadGrant flags a grant that could not be expanded to an
// explicit path (e.g. ".", "*", "./"). Emits one MEDIUM finding per
// known secret basename glob.
func checkBroadGrant(field, entry string) []Finding {
	var findings []Finding
	for _, g := range SecretBasenameGlobs {
		findings = append(findings, Finding{
			Severity: SeverityMedium,
			Category: CatFSGrant,
			Field:    field,
			Value:    entry,
			Message:  "broad grant may expose \"" + g + "\" files",
		})
	}
	return findings
}

// isParent reports whether parent == child or child is beneath parent.
func isParent(parent, child string) bool {
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

// checkNetwork is a stub; implemented in Task 5.
func checkNetwork(profile *sandboxprofile.Profile) []Finding {
	return nil
}
