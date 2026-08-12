// Package skillstate answers one question — "is this skill ready to mount?" —
// and answers it exactly once for the whole codebase.
//
// Resolving a registered skill's declared secrets and config fields, then
// deciding whether it can be armed, used to be implemented independently in
// six places (`omac start`'s preflight and its auto-register eligibility
// check, `omac serve`'s bringUp, start's live-reload loop, `omac doctor`, and
// `omac config show`). The copies drifted, and the drift was silent: a skill
// whose only required field came from $default_from_env started fine under
// start and serve but was skipped forever by live reload; a secret listed in
// both `secrets:` and `env_passthrough:` — the documented fallback for
// keychain-less CI runners — was rejected by serve even though the supervisor
// would have injected it at spawn; doctor probed the keychain unscoped while
// start probed it workdir-scoped, so the two disagreed about the same secret.
// See issue #174.
//
// The split of responsibility is: this package owns the RULE and emits
// []Problem; every caller owns only the PRESENTATION of those problems —
// start renders a consolidated refusal and picks an exit code, serve installs
// a pending-credentials or broken route, reload does the same on the live
// facade, doctor prints warnings, `config show` renders a source column.
// Adding a resolution rung (a new fallback, a new precedence step) is a change
// here and nowhere else.
package skillstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
)

// ProblemKind classifies why a skill is not ready. Callers switch on it to
// choose a presentation (refusal section, route state, warning line) and, for
// start, an exit code.
type ProblemKind string

const (
	// MetaBroken means omac.yaml is missing, unparseable, or no longer
	// declares a sidecar block. Not fixable by supplying a value.
	MetaBroken ProblemKind = "meta-broken"
	// BundleDrift means the skill directory's content hash no longer matches
	// what was recorded at register time, and the caller did not opt into
	// accepting that.
	BundleDrift ProblemKind = "bundle-drift"
	// MissingSecret means a required secret is in neither the keychain nor
	// (via env_passthrough) the host environment.
	MissingSecret ProblemKind = "missing-secret"
	// InvalidSecret means a secret supplied from the host environment failed
	// its declared pattern. Keychain values were vetted at register time, so
	// this only ever arises for env_passthrough-supplied values.
	InvalidSecret ProblemKind = "invalid-secret"
	// KeychainUnavailable means the keychain could not answer for a required
	// secret: the backend is missing (headless Linux/WSL with no Secret
	// Service), or it failed opaquely (a macOS authorization denial, a corrupt
	// entry). Distinct from MissingSecret because "run omac secrets set" is
	// useless advice when there is no working keychain to set it in. Problem.Fix
	// carries the OS-specific remedy when the backend is provably missing.
	KeychainUnavailable ProblemKind = "keychain-unavailable"
	// MissingField means a required config field resolved to no value through
	// any rung of the precedence ladder.
	MissingField ProblemKind = "missing-field"
)

// Problem is one reason a skill is not ready.
type Problem struct {
	Kind  ProblemKind
	Skill string
	// Field is the secret or config field name; empty for skill-level
	// problems (MetaBroken, BundleDrift).
	Field string
	// Detail is the human-readable cause.
	Detail string
	// Fix is the `omac …` command (or other concrete remedy) that resolves
	// it. Written once here so every caller renders the same remediation.
	// Empty when there is no known remedy (an opaque keychain failure).
	Fix string
	// Cause is the underlying error, when one exists, for callers that need to
	// classify it rather than print it — auto-register eligibility declines
	// silently on a missing backend (errors.Is ErrNotFound) but propagates an
	// opaque keychain failure. Nil for problems derived from absent values.
	Cause error
}

// Source names the rung of the precedence ladder a resolved value came from,
// or why it is absent.
//
// These strings are part of `omac config show`'s output contract: they appear
// in the --json payload and `omac config get` switches on the two missing-*
// values, so they must not be renamed casually.
type Source string

const (
	SourceStored         Source = "stored"           // skill-config.yaml (workdir or global layer)
	SourceDefault        Source = "default"          // omac.yaml `default:`
	SourceKeychain       Source = "keychain"         // OS keychain, scoped or unscoped
	SourceEnvPassthrough Source = "env_passthrough"  // host env, because the secret is listed in env_passthrough
	SourceMissingReq     Source = "missing-required" // required and unresolved
	SourceMissingOpt     Source = "missing-optional" // optional and unresolved
)

// SourceDefaultFromEnv renders the `default_from_env` rung, which names the
// variable it read so `config show` can display which one won.
func SourceDefaultFromEnv(envVar string) Source { return Source("default_from_env:" + envVar) }

// Options configures a Resolver. The zero value resolves unscoped secrets
// against the real keychain and the real process environment.
type Options struct {
	// Scope is the keychain scope: a workdir-id for workdir-local skills, or
	// "" for user-global skills and legacy single-workdir start. Lookups
	// always fall back to the unscoped key (see keychain.GetWithFallback), so
	// a secret stored either way resolves.
	Scope string

	// Env reads the host environment. nil means os.LookupEnv. Injected by
	// tests so an env_passthrough or default_from_env case does not have to
	// mutate the process.
	Env func(string) (string, bool)

	// AcceptBundleDrift suppresses BundleDrift problems (`--accept-skill-changes`).
	AcceptBundleDrift bool

	// SkipSecretPattern suppresses InvalidSecret problems
	// (`--skip-secret-pattern`), the escape hatch for a stale pattern in a
	// skill's omac.yaml. The raw value is still passed through to the sidecar.
	SkipSecretPattern bool

	// SkipBundleHash skips hashing the skill directory entirely. Set by
	// callers that only report on values (doctor, `config show`) or for which
	// no recorded hash exists yet (auto-register eligibility), so they don't
	// pay a tree walk they have no use for.
	SkipBundleHash bool

	// Keychain reads a secret. nil means keychain.GetWithFallback. Injected by
	// tests to simulate a dead backend without a process-global mock.
	Keychain func(scope, skill, name string) (secrets.Secret, error)
}

// Armed is everything a caller needs to spawn a sidecar for one skill. It is
// returned even when there are problems (partially populated) so a caller can
// still report on what DID resolve — `config show` renders exactly that.
//
// Armed owns live secret material. Every path that obtains one, including
// early returns on the problem paths, must Zero it.
type Armed struct {
	Entry  registry.Entry
	Meta   *config.Meta
	AbsDir string
	// Mount is the facade mount point (sidecar.mount, defaulting to the
	// skill name).
	Mount string
	// Bundle is AbsDir's content hash, computed once here so callers that
	// need it for both the drift check and the spawn-approval gate don't walk
	// the tree twice. Empty when SkipBundleHash or when hashing failed.
	Bundle string
	// BundleErr is a hashing I/O failure, kept distinct from BundleDrift
	// because callers treat them differently: start aborts the whole launch
	// (it cannot produce useful per-skill diagnostics for an unreadable
	// directory), while serve leaves the hash empty for the approval gate to
	// re-derive and refuse.
	BundleErr error

	Secrets map[string]secrets.Secret
	Config  map[string]string

	// SecretSources / ConfigSources record which rung produced each value,
	// including the missing-* markers for unresolved ones. Presentation-only;
	// the spawn path needs just Secrets and Config.
	SecretSources map[string]Source
	ConfigSources map[string]Source
}

// Zero wipes every resolved secret's plaintext. Safe on a zero-value Armed and
// safe to call twice, so `defer armed.Zero()` immediately after resolving is
// always correct.
func (a *Armed) Zero() {
	for name := range a.Secrets {
		s := a.Secrets[name]
		s.Zero()
		a.Secrets[name] = s
	}
}

// Resolver applies the readiness rule to one or more skills. Construct one per
// pass (a launch preflight, a reload sweep, a doctor run) so a dead keychain is
// discovered once rather than per skill — see the sticky behaviour in
// resolveSecrets.
type Resolver struct {
	opts Options
	// keychainDown records that the backend answered with a hard failure or
	// reported itself unavailable while a required secret still needed it.
	// Once set, later skills in this pass skip the backend entirely: without
	// it, a macOS run that lost keychain authorization would raise one
	// blocking auth prompt per skill, and start's pre-#174 behaviour of
	// aborting on the first keychain error would be replaced by N failures.
	keychainDown error
}

// New returns a Resolver applying o.
func New(o Options) *Resolver { return &Resolver{opts: o} }

// Resolve applies the readiness rule to a skill whose meta is already loaded.
//
// absDir is the skill's own directory, already absolute. cfg is the merged
// (global + workdir) config store — see MergeConfig.
func (r *Resolver) Resolve(m *config.Meta, e registry.Entry, absDir string, cfg *skillconfig.Store) (Armed, []Problem) {
	armed := Armed{
		Entry:         e,
		Meta:          m,
		AbsDir:        absDir,
		Mount:         m.Sidecar.MountOrDefault(e.Name),
		Secrets:       map[string]secrets.Secret{},
		Config:        map[string]string{},
		SecretSources: map[string]Source{},
		ConfigSources: map[string]Source{},
	}
	var problems []Problem

	if !r.opts.SkipBundleHash {
		bundle, err := config.BundleHash(absDir)
		armed.Bundle, armed.BundleErr = bundle, err
		// Report drift only when the hash is trustworthy; a hashing failure
		// is the caller's to interpret via BundleErr.
		if err == nil && !r.opts.AcceptBundleDrift && bundle != e.BundleHash {
			problems = append(problems, Problem{
				Kind:   BundleDrift,
				Skill:  e.Name,
				Detail: "bundle changed since register",
				Fix:    "omac register --force " + e.Name,
			})
		}
	}

	problems = append(problems, r.resolveSecrets(&armed)...)
	problems = append(problems, r.resolveConfig(&armed, cfg)...)
	return armed, problems
}

// Load is Resolve preceded by reading omac.yaml. A meta that is missing,
// unparseable, or sidecar-less yields a single MetaBroken problem and a
// zero-value Armed, since nothing further can be resolved.
func (r *Resolver) Load(e registry.Entry, absDir string, cfg *skillconfig.Store) (Armed, []Problem) {
	m, err := config.LoadMeta(filepath.Join(absDir, config.MetaFileName))
	if err != nil {
		return Armed{Entry: e, AbsDir: absDir, Mount: e.Name}, []Problem{{
			Kind:   MetaBroken,
			Skill:  e.Name,
			Detail: err.Error(),
			Fix:    "omac register --force " + e.Name,
			Cause:  err,
		}}
	}
	if m.Sidecar == nil {
		return Armed{Entry: e, Meta: m, AbsDir: absDir, Mount: e.Name}, []Problem{{
			Kind:   MetaBroken,
			Skill:  e.Name,
			Detail: config.MetaFileName + " no longer has a sidecar block",
			Fix:    "omac register --force " + e.Name,
		}}
	}
	return r.Resolve(m, e, absDir, cfg)
}

// resolveSecrets fills armed.Secrets. Precedence, and the order matters:
//
//  1. the keychain (scoped, falling back to unscoped);
//  2. else the host environment, but ONLY for a secret listed in
//     env_passthrough — the documented fallback for keychain-less
//     environments (config/meta.go's SidecarMeta docs; the supervisor injects
//     these at spawn, see supervisor.buildEnv). An env-supplied value is
//     re-validated against the spec's pattern, because unlike a keychain value
//     it was never vetted at register time;
//  3. else a problem, if the secret is required.
//
// Step 2 runs even when the keychain backend is DOWN, and that is deliberate:
// keychain.GetScoped reports an unavailable backend as ErrNotFound (plus
// ErrUnavailable), so a headless CI runner exporting its credentials in the
// shell resolves them here exactly as before. Only once no fallback has
// satisfied a required secret does the ErrUnavailable classification matter —
// at which point reporting "no Secret Service provider" instead of "run omac
// secrets set" is the whole point of issue #174's Failure 4.
//
// spec.DefaultFromEnv is deliberately NOT consulted for secrets. register.go
// honours it when prompting, but the supervisor only injects variables named
// in env_passthrough, so accepting it here would arm a skill whose sidecar
// then starts without the value.
func (r *Resolver) resolveSecrets(armed *Armed) []Problem {
	specs := armed.Meta.Sidecar.Secrets
	if len(specs) == 0 {
		return nil
	}
	passthrough := map[string]struct{}{}
	for _, name := range armed.Meta.Sidecar.EnvPassthrough {
		passthrough[name] = struct{}{}
	}

	var problems []Problem
	for _, spec := range specs {
		val, err := r.getSecret(armed.Entry.Name, spec.Name)
		if err == nil {
			armed.Secrets[spec.Name] = val
			armed.SecretSources[spec.Name] = SourceKeychain
			continue
		}

		if envVal, ok := r.secretFromEnv(spec.Name, passthrough); ok {
			armed.Secrets[spec.Name] = secrets.NewSecretString(envVal)
			armed.SecretSources[spec.Name] = SourceEnvPassthrough
			if !r.opts.SkipSecretPattern {
				if perr := spec.ValidateValue(envVal); perr != nil {
					problems = append(problems, Problem{
						Kind:   InvalidSecret,
						Skill:  armed.Entry.Name,
						Field:  spec.Name,
						Detail: perr.Error(),
						Fix: fmt.Sprintf("fix the exported value, or run omac secrets set %s %s",
							armed.Entry.Name, spec.Name),
					})
				}
			}
			continue
		}

		if !spec.IsRequired() {
			armed.SecretSources[spec.Name] = SourceMissingOpt
			continue
		}
		armed.SecretSources[spec.Name] = SourceMissingReq
		problems = append(problems, r.secretProblem(armed.Entry.Name, spec.Name, err))
	}
	return problems
}

// secretProblem classifies why a required secret could not be resolved.
func (r *Resolver) secretProblem(skill, name string, err error) Problem {
	unavailable := errors.Is(err, keychain.ErrUnavailable) || keychain.IsUnavailable(err)
	if unavailable || !errors.Is(err, keychain.ErrNotFound) {
		// Either the backend reported itself missing, or it failed in a way we
		// cannot interpret. Both are environment problems, not "you forgot to
		// set it". Only the former has an actionable OS-specific remedy — an
		// opaque failure gets no Fix rather than a misleading "install
		// gnome-keyring" on a Mac.
		p := Problem{
			Kind:  KeychainUnavailable,
			Skill: skill,
			Field: name,
			// BackendCause, not err: the full chain leads with "secret not
			// found", which contradicts the section this problem renders under.
			Detail: keychain.BackendCause(err).Error(),
			Cause:  err,
		}
		if unavailable {
			p.Fix = keychain.UnavailableHint(osinfo.Detect())
		}
		return p
	}
	return Problem{
		Kind:   MissingSecret,
		Skill:  skill,
		Field:  name,
		Detail: "required secret missing",
		Fix:    fmt.Sprintf("omac secrets set %s %s", skill, name),
	}
}

// getSecret reads one secret, short-circuiting once the backend has proven
// itself broken for this pass.
func (r *Resolver) getSecret(skill, name string) (secrets.Secret, error) {
	if r.keychainDown != nil {
		return secrets.Secret{}, r.keychainDown
	}
	get := r.opts.Keychain
	if get == nil {
		get = keychain.GetWithFallback
	}
	val, err := get(r.opts.Scope, skill, name)
	if err == nil {
		return val, nil
	}
	// A plain absent secret says nothing about the backend's health; anything
	// else does, and there is no point asking it again for every later skill.
	if !errors.Is(err, keychain.ErrNotFound) || errors.Is(err, keychain.ErrUnavailable) {
		r.keychainDown = err
	}
	return secrets.Secret{}, err
}

// secretFromEnv returns the host value that will satisfy a keychain-absent
// secret at runtime, if any. ok is true only when the secret is listed under
// sidecar.env_passthrough AND the host exports a non-empty value for it.
//
// The supervisor passes env_passthrough vars through whenever they are present
// even if empty; an empty token is no token, so an empty value deliberately
// does not satisfy a required secret here.
func (r *Resolver) secretFromEnv(name string, passthrough map[string]struct{}) (string, bool) {
	if _, ok := passthrough[name]; !ok {
		return "", false
	}
	v, ok := r.lookupEnv(name)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// resolveConfig fills armed.Config with precedence
// stored > spec.Default > $spec.DefaultFromEnv > missing.
//
// Config VALUES are not re-validated against spec.Pattern/Choices: that is
// register-time prompting behaviour and moving it here would newly refuse
// skills that start fine today.
func (r *Resolver) resolveConfig(armed *Armed, cfg *skillconfig.Store) []Problem {
	var problems []Problem
	for _, spec := range armed.Meta.Sidecar.Config {
		if cfg != nil {
			if v, ok := cfg.Get(armed.Entry.Name, spec.Name); ok {
				armed.Config[spec.Name] = v
				armed.ConfigSources[spec.Name] = SourceStored
				continue
			}
		}
		if spec.Default != "" {
			armed.Config[spec.Name] = spec.Default
			armed.ConfigSources[spec.Name] = SourceDefault
			continue
		}
		if spec.DefaultFromEnv != "" {
			if v, ok := r.lookupEnv(spec.DefaultFromEnv); ok && v != "" {
				armed.Config[spec.Name] = v
				armed.ConfigSources[spec.Name] = SourceDefaultFromEnv(spec.DefaultFromEnv)
				continue
			}
		}
		if !spec.IsRequired() {
			armed.ConfigSources[spec.Name] = SourceMissingOpt
			continue
		}
		armed.ConfigSources[spec.Name] = SourceMissingReq
		problems = append(problems, Problem{
			Kind:   MissingField,
			Skill:  armed.Entry.Name,
			Field:  spec.Name,
			Detail: "required config field missing",
			Fix:    "omac register --reprompt-fields " + armed.Entry.Name,
		})
	}
	return problems
}

func (r *Resolver) lookupEnv(name string) (string, bool) {
	if r.opts.Env != nil {
		return r.opts.Env(name)
	}
	return os.LookupEnv(name)
}

// MissingFields returns the sorted names of the secrets and config fields that
// are required but unresolved, for callers that report "missing credentials"
// as a list (serve's and reload's pending-credentials routes).
func MissingFields(problems []Problem) []string {
	var out []string
	for _, p := range problems {
		if p.Kind == MissingSecret || p.Kind == MissingField {
			out = append(out, p.Field)
		}
	}
	sort.Strings(out)
	return out
}

// Has reports whether problems contains any of kinds.
func Has(problems []Problem, kinds ...ProblemKind) bool {
	for _, p := range problems {
		for _, k := range kinds {
			if p.Kind == k {
				return true
			}
		}
	}
	return false
}

// First returns the first problem of any of kinds, or nil.
func First(problems []Problem, kinds ...ProblemKind) *Problem {
	for i, p := range problems {
		for _, k := range kinds {
			if p.Kind == k {
				return &problems[i]
			}
		}
	}
	return nil
}

// MergeConfig returns a store whose (skill, field) values are the union of the
// global and workdir layers, with workdir values overriding global ones
// field-by-field. Neither input is mutated.
//
// It lives here because it is the first rung of the precedence ladder
// resolveConfig implements: "stored" means "stored in this merge".
func MergeConfig(global, workdir *skillconfig.Store) *skillconfig.Store {
	out := &skillconfig.Store{Version: skillconfig.SchemaVersion, Skills: map[string]map[string]string{}}
	for skill, fields := range global.Skills {
		for field, val := range fields {
			out.Set(skill, field, val)
		}
	}
	for skill, fields := range workdir.Skills {
		for field, val := range fields {
			out.Set(skill, field, val)
		}
	}
	return out
}
