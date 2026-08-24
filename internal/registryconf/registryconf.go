// Package registryconf derives a credential-free projection of a package
// manager's user config file, so a sandboxed toolchain can resolve
// scope→registry mappings without the host file ever being readable.
//
// The problem it solves: a scoped package like @acme/foo resolves through
// the `@acme:registry=` line in ~/.npmrc. That file is a protected path
// (internal/sandboxprofile/baseline.go, protectedCommon) because it also
// commonly holds `_authToken`. Masking it entirely makes npm fall back to
// the public registry, where the package does not exist — the install
// fails with a 404 that reads like "no such package" rather than "your
// registry configuration is invisible". See #150 / #241.
//
// No credential can survive by construction: only registry-mapping keys
// are kept, a kept value must parse as an http(s) URL, and any userinfo
// in that URL is removed.
package registryconf

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// Projection is one scrubbed config file written for the sandboxed child.
type Projection struct {
	// Ecosystem is the sandboxprofile.RegistryConfigTools identifier.
	Ecosystem string
	// Source is the host file the projection was derived from.
	Source string
	// Path is the scrubbed file. Grant read access to exactly this path.
	Path string
	// EnvVar points the tool at Path (e.g. NPM_CONFIG_USERCONFIG).
	EnvVar string
	// KeptKeys are the registry-mapping keys that survived the scrub.
	KeptKeys []string
	// Dropped counts removed non-comment entries, credentials included.
	Dropped int
	// StrippedUserinfo counts kept mappings whose URL carried inline
	// credentials (https://user:pass@host) that were removed.
	StrippedUserinfo int
}

// Summary renders a one-line, secret-free description for the launch log.
func (p Projection) Summary() string {
	extra := ""
	if p.StrippedUserinfo > 0 {
		extra = fmt.Sprintf(", %d inline credential(s) stripped", p.StrippedUserinfo)
	}
	return fmt.Sprintf("%s: projected %d registry mapping(s) from %s (%d other entr(ies) dropped%s)",
		p.Ecosystem, len(p.KeptKeys), p.Source, p.Dropped, extra)
}

// projector derives one ecosystem's projection into dir. A missing host
// config is not an error: it returns ok=false and no projection.
type projector func(dir string) (Projection, bool, error)

// projectors maps each registry_config ecosystem to its implementation.
// Adding an ecosystem (.pypirc, cargo credentials) is one entry here plus
// one in sandboxprofile.RegistryConfigTools; TestProjectorsCoverProfileTools
// asserts the two stay in sync.
var projectors = map[string]projector{
	sandboxprofile.RegistryConfigNPM: projectNPM,
}

// Project writes a scrubbed config for every requested ecosystem into dir
// and returns what it produced. Ecosystems are pre-validated by
// sandboxprofile.Profile.Validate, so an unknown one is a programming
// error rather than user input.
func Project(ecosystems []string, dir string) ([]Projection, error) {
	var out []Projection
	for _, eco := range ecosystems {
		proj, ok := projectors[eco]
		if !ok {
			return nil, fmt.Errorf("registry_config: no projector for %q", eco)
		}
		p, present, err := proj(dir)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// NPMUserConfig returns the host file npm reads its user config from.
// Exported so the doctor-side detector inspects exactly the same path the
// projector would read.
func NPMUserConfig() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".npmrc"), nil
}

func projectNPM(dir string) (Projection, bool, error) {
	src, err := NPMUserConfig()
	if err != nil {
		return Projection{}, false, fmt.Errorf("registry_config npm: %w", err)
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return Projection{}, false, nil
		}
		return Projection{}, false, fmt.Errorf("registry_config npm: read %s: %w", src, err)
	}
	res := ScrubNPMRC(raw)
	if len(res.KeptKeys) == 0 {
		// No mapping means npm's default resolution is already correct;
		// writing an empty file would add a grant for no reason.
		return Projection{}, false, nil
	}
	dest := filepath.Join(dir, "npmrc")
	if err := os.WriteFile(dest, res.Content, 0o600); err != nil {
		return Projection{}, false, fmt.Errorf("registry_config npm: write projection: %w", err)
	}
	return Projection{
		Ecosystem:        sandboxprofile.RegistryConfigNPM,
		Source:           src,
		Path:             dest,
		EnvVar:           "NPM_CONFIG_USERCONFIG",
		KeptKeys:         res.KeptKeys,
		Dropped:          res.Dropped,
		StrippedUserinfo: res.StrippedUserinfo,
	}, true, nil
}

// defaultNPMRegistryHost is npm's built-in registry. A mapping that points
// here needs no projection: it is what npm would do anyway.
const defaultNPMRegistryHost = "registry.npmjs.org"

// Notice reports that a projection would change the outcome of a launch.
// It is advisory, produced by inspection rather than at launch time, so
// `omac doctor` can explain a 404 the user has not hit yet.
type Notice struct {
	// Ecosystem is the registry_config identifier that would apply.
	Ecosystem string
	// Source is the host config file inspected.
	Source string
	// Hosts are the non-default registry hosts the file maps to.
	Hosts []string
	// Credentialed is true when the file also holds auth entries, which
	// is why override_deny is the wrong remedy.
	Credentialed bool
	// Enabled reflects whether the profile already opts in.
	Enabled bool
	// Overridden reflects whether override_deny already exposes Source.
	Overridden bool
}

// InspectNPM reports whether ~/.npmrc maps any scope to a non-default
// registry, and how the given profile currently handles it. It returns nil
// when there is nothing to say: no file, no mapping, or every mapping
// already points at npm's default registry.
//
// The predicate is deliberately "host is not npm's default" rather than
// netproxy's isPackageRegistry heuristic: that heuristic answers a
// different question (does a *host* look like a registry?) and returns
// false for exactly the corporate shape this detector exists for —
// e.g. tng-artifacts.int.tngtech.com has no "registry"/"npm" label.
func InspectNPM(enabled, overridden bool) (*Notice, error) {
	src, err := NPMUserConfig()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	res := ScrubNPMRC(raw)
	var hosts []string
	for _, line := range strings.Split(strings.TrimSpace(string(res.Content)), "\n") {
		_, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(value))
		if err != nil || u.Hostname() == "" || u.Hostname() == defaultNPMRegistryHost {
			continue
		}
		if !slices.Contains(hosts, u.Hostname()) {
			hosts = append(hosts, u.Hostname())
		}
	}
	if len(hosts) == 0 {
		return nil, nil
	}
	return &Notice{
		Ecosystem:    sandboxprofile.RegistryConfigNPM,
		Source:       src,
		Hosts:        hosts,
		Credentialed: res.DroppedCredentials > 0,
		Enabled:      enabled,
		Overridden:   overridden,
	}, nil
}

// ScrubResult is the outcome of scrubbing one config file.
type ScrubResult struct {
	// Content is the projected file body (mapping lines only), empty when
	// nothing survived.
	Content []byte
	// KeptKeys are the config keys that survived.
	KeptKeys []string
	// Dropped counts removed non-comment entries.
	Dropped int
	// DroppedCredentials counts the subset of Dropped whose key carries
	// authentication material. Reported separately because "this file
	// holds a token" changes the advice: override_deny would expose it,
	// a projection does not.
	DroppedCredentials int
	// StrippedUserinfo counts kept URLs that carried inline credentials.
	StrippedUserinfo int
}

// scopedRegistryKey matches npm's scoped-registry form, e.g.
// "@acme:registry" or "@acme/sub:registry".
var scopedRegistryKey = regexp.MustCompile(`^@[^:\s]+:registry$`)

// credentialKey matches npmrc keys that carry authentication material,
// including the per-registry form "//host/path/:_authToken".
var credentialKey = regexp.MustCompile(`(?i)(_auth|_authtoken|_password|username|email|^//)`)

// ScrubNPMRC keeps only registry mappings from an npmrc body. Everything
// else — credentials, comments, unrelated knobs — is dropped.
func ScrubNPMRC(src []byte) ScrubResult {
	var res ScrubResult
	var lines []string
	for _, raw := range strings.Split(string(src), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			res.Dropped++
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !isRegistryMapping(key) {
			res.Dropped++
			if credentialKey.MatchString(key) {
				res.DroppedCredentials++
			}
			continue
		}
		clean, stripped, ok := registryURL(value)
		if !ok {
			// A mapping whose value is not an http(s) URL is not something
			// we can vouch for; dropping is the safe direction.
			res.Dropped++
			continue
		}
		if stripped {
			res.StrippedUserinfo++
		}
		lines = append(lines, key+"="+clean)
		res.KeptKeys = append(res.KeptKeys, key)
	}
	if len(lines) > 0 {
		res.Content = []byte(strings.Join(lines, "\n") + "\n")
	}
	return res
}

// isRegistryMapping reports whether key is a registry mapping: the global
// `registry` or a scoped `@scope:registry`. Comparison is case-insensitive
// because npm lowercases config keys.
func isRegistryMapping(key string) bool {
	k := strings.ToLower(key)
	return k == "registry" || scopedRegistryKey.MatchString(k)
}

// registryURL validates a mapping value and removes any credentials
// embedded in it. ok=false means the value is not an absolute http(s) URL
// and must not be projected. Note that a hostname merely *containing*
// "token" (api.trustedtokens.eu) is a legitimate registry and is kept —
// the credential check is structural (userinfo), not textual.
func registryURL(value string) (clean string, stripped bool, ok bool) {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return "", false, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false, false
	}
	if u.User != nil {
		u.User = nil
		return u.String(), true, true
	}
	return u.String(), false, true
}
