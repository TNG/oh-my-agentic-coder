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
	// Rejected lists mappings omac recognized but refused to project.
	Rejected []Rejected
	// NeedsAuth lists projected mapping keys whose host has a credential
	// entry omac deliberately did not copy.
	NeedsAuth []string
	// Warning explains why no projection was produced, when the reason is
	// worth telling the user about (an unreadable config rather than an
	// absent one). Path is empty in that case.
	Warning string
}

// Projected reports whether a usable projection was written. A Projection
// with Projected()==false may still carry a Warning or Rejected entries the
// caller should surface.
func (p Projection) Projected() bool { return p.Path != "" }

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
// config is not an error: it returns present=false. A returned Projection
// may still carry a Warning or Rejected entries when present=false, so the
// caller can explain why nothing was projected.
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
//
// Entries with Projected()==false are still returned when they carry a
// Warning or Rejected mappings: the caller must be able to say why a
// requested projection produced nothing, since silence there reproduces
// the very 404 this package prevents.
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
		if !present && p.Warning == "" && len(p.Rejected) == 0 {
			continue
		}
		if p.Ecosystem == "" {
			p.Ecosystem = eco
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
		// Nothing to project and nothing omac can do about it. This is an
		// opt-in convenience that only ever ADDS a mapping, so it must not
		// take the whole launch down (a systemd unit or minimal container
		// with no resolvable home would otherwise never start).
		return Projection{Warning: fmt.Sprintf("cannot locate the npm user config: %v", err)}, false, nil
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return Projection{}, false, nil
		}
		// Same reasoning as above: EACCES on ~/.npmrc (e.g. root-owned
		// after a sudo npm config set) is a warning, not a fatal error.
		return Projection{Warning: fmt.Sprintf("cannot read %s: %v", src, err)}, false, nil
	}
	res := ScrubNPMRC(raw)
	if len(res.KeptKeys) == 0 {
		// No mapping means npm's default resolution is already correct;
		// writing an empty file would add a grant for no reason. Rejections
		// still travel back so the caller can explain the silence.
		return Projection{Source: src, Rejected: res.Rejected}, false, nil
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
		Rejected:         res.Rejected,
		NeedsAuth:        res.NeedsAuth,
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
	// Rejected lists mappings that exist in the file but cannot be
	// projected. Carried so doctor reports them instead of staying silent
	// while scoped installs keep 404ing.
	Rejected []Rejected
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
	// A rejected mapping is exactly the case that must not be silent: the
	// file has private-registry config, the sandbox cannot use it, and the
	// user would otherwise get an unexplained 404.
	if len(hosts) == 0 && len(res.Rejected) == 0 {
		return nil, nil
	}
	return &Notice{
		Ecosystem:    sandboxprofile.RegistryConfigNPM,
		Source:       src,
		Hosts:        hosts,
		Credentialed: res.DroppedCredentials > 0,
		Enabled:      enabled,
		Overridden:   overridden,
		Rejected:     res.Rejected,
	}, nil
}

// Rejected is a registry mapping omac recognized but refused to project,
// with the reason. These are reported rather than silently skipped: a
// mapping that does not reach the sandbox leaves the exact 404 this package
// exists to prevent, so silence would recreate the original bug.
type Rejected struct {
	Key    string
	Reason string
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
	// Rejected lists recognized mappings that could not be projected.
	Rejected []Rejected
	// NeedsAuth lists kept mapping keys whose registry host has a
	// credential entry in the source file. omac cannot supply that
	// credential (it is exactly what the projection drops), so installs
	// against those hosts may fail authentication.
	NeedsAuth []string
}

// scopedRegistryKey matches npm's scoped-registry form, e.g.
// "@acme:registry" or "@acme/sub:registry".
var scopedRegistryKey = regexp.MustCompile(`^@[^:\s]+:registry$`)

// credentialKey matches npmrc keys that carry authentication material,
// including the per-registry form "//host/path/:_authToken".
var credentialKey = regexp.MustCompile(`(?i)(_auth|_authtoken|_password|username|email|^//)`)

// ScrubNPMRC keeps only registry mappings from an npmrc body. Everything
// else — credentials, comments, unrelated knobs — is dropped.
//
// A mapping is kept only if its value resolves to a credential-free
// http(s) URL. npm's own value syntax is honored first (surrounding quotes
// are stripped, ${VAR} is expanded from the environment), so a mapping npm
// would act on is not silently lost. Anything still unusable — or carrying
// a secret outside the userinfo, e.g. ?apiKey= — is recorded in Rejected
// rather than dropped quietly.
func ScrubNPMRC(src []byte) ScrubResult {
	var res ScrubResult
	var lines []string
	// keptHosts maps a kept mapping key to its registry host, so the
	// credential correlation below can run after the whole file is read
	// (auth lines may appear before or after the mapping they apply to).
	keptHosts := map[string]string{}
	authHosts := map[string]bool{}

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
				if h := credentialHost(key); h != "" {
					authHosts[h] = true
				}
			}
			continue
		}
		clean, host, stripped, reason := registryURL(npmValue(value))
		if reason != "" {
			res.Rejected = append(res.Rejected, Rejected{Key: key, Reason: reason})
			continue
		}
		if stripped {
			res.StrippedUserinfo++
		}
		lines = append(lines, key+"="+clean)
		res.KeptKeys = append(res.KeptKeys, key)
		keptHosts[key] = host
	}

	// Correlate kept mappings with the credentials that were dropped. The
	// global `registry` key is special: pointing npm at a private mirror it
	// cannot authenticate to breaks *every* install, including the public
	// dependencies that work today with the file masked. That is a
	// regression, so it is refused rather than projected. A scoped mapping
	// only affects its own scope, which was already failing, so it is kept
	// with a warning.
	var keptLines []string
	var keptKeys []string
	for i, key := range res.KeptKeys {
		host := keptHosts[key]
		if authHosts[host] {
			if strings.EqualFold(key, "registry") {
				res.Rejected = append(res.Rejected, Rejected{
					Key: key,
					Reason: fmt.Sprintf("%s requires authentication that omac cannot supply; projecting the global registry "+
						"would redirect every install there and break the public ones that work today", host),
				})
				continue
			}
			res.NeedsAuth = append(res.NeedsAuth, key)
		}
		keptKeys = append(keptKeys, key)
		keptLines = append(keptLines, lines[i])
	}
	res.KeptKeys = keptKeys
	if len(keptLines) > 0 {
		res.Content = []byte(strings.Join(keptLines, "\n") + "\n")
	}
	return res
}

// npmValue applies npm's ini value syntax before the URL is validated:
// surrounding quotes are removed and ${VAR}/$VAR are expanded from the
// environment, both of which npm does itself. Without this a perfectly
// good mapping (`@acme:registry="https://npm.acme.test"`) would look
// unparseable and be silently skipped.
func npmValue(value string) string {
	v := strings.TrimSpace(value)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	return os.ExpandEnv(v)
}

// credentialHost extracts the registry host from a per-registry auth key
// such as "//npm.acme.test/:_authToken" or "//npm.acme.test/api/:_password".
// Returns "" for keys that are not host-scoped (e.g. a bare "_auth").
func credentialHost(key string) string {
	rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(key)), "//")
	if !ok {
		return ""
	}
	host, _, _ := strings.Cut(rest, "/")
	return host
}

// isRegistryMapping reports whether key is a registry mapping: the global
// `registry` or a scoped `@scope:registry`. Comparison is case-insensitive
// because npm lowercases config keys.
func isRegistryMapping(key string) bool {
	k := strings.ToLower(key)
	return k == "registry" || scopedRegistryKey.MatchString(k)
}

// registryURL validates a mapping value and removes credentials embedded in
// it. A non-empty reason means the value must not be projected. Note that a
// hostname merely *containing* "token" (api.trustedtokens.eu) is a
// legitimate registry and is kept — the credential checks are structural,
// not textual.
//
// A query string or fragment is refused outright rather than stripped: it
// is a common place to carry an API key (`?apiKey=…`), and omac cannot tell
// a secret query parameter from a load-bearing one. Stripping it might
// silently change resolution; keeping it would leak the secret into a
// sandbox-readable file. Refusing says so instead.
func registryURL(value string) (clean, host string, stripped bool, reason string) {
	u, err := url.Parse(value)
	if err != nil {
		return "", "", false, fmt.Sprintf("value %q is not a URL", value)
	}
	if u.Host == "" {
		return "", "", false, fmt.Sprintf("value %q has no host", value)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", false, fmt.Sprintf("scheme %q is not http(s)", u.Scheme)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", "", false, "URL carries a query string, which commonly holds an API key omac must not copy into the sandbox"
	}
	if u.Fragment != "" {
		return "", "", false, "URL carries a fragment"
	}
	if u.User != nil {
		u.User = nil
		stripped = true
	}
	return u.String(), u.Hostname(), stripped, ""
}
