package cli

// `omac config` is the host-side counterpart of a sidecar's /whoami
// route: it shows what omac WOULD inject into the sidecar's
// environment if omac start ran right now, without actually spawning
// any sidecars or connecting to anything.
//
// Two subcommands:
//
//   omac config show <skill>            human + --json
//   omac config get  <skill> <field>    one value, one line
//
// Resolution order (mirrors register.go and start.go):
//
//   1. Value persisted in <workdir>/.opencode/skill-config.yaml.
//   2. Else `default:` from omac.yaml.
//   3. Else, for ConfigSpec, `default_from_env: <VAR>` if the var is
//      set in the SHELL THAT IS RUNNING `omac config` (NOT the same
//      shell that registered the skill — surface this distinction in
//      the source column so users don't get confused).
//   4. Else <missing-required> or <missing-optional> as appropriate.
//
// Secrets are surfaced as sha256(value)[:12] fingerprints, byte-for-
// byte identical to what echo-rest's /whoami currently prints. The
// plaintext briefly lives in this process's address space (same as
// during omac start) and is zeroed before the command exits.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillstate"
)

// runConfig dispatches "omac config <subcommand>".
func runConfig(args []string, env *Env) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "Usage: omac config <show|get> <skill> [args]")
		return ExitMisuse
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "show":
		return runConfigShow(rest, env)
	case "get":
		return runConfigGet(rest, env)
	default:
		fmt.Fprintf(env.Stderr, "omac config: unknown subcommand %q\n", sub)
		return ExitMisuse
	}
}

// runConfigShow prints every config field and every declared secret
// (as a fingerprint) for a registered skill.
func runConfigShow(args []string, env *Env) int {
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	jsonOut := fs.Bool("json", false, "Emit a single JSON object instead of tabular text.")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac config show <skill> [--json]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return ExitMisuse
	}
	skill := fs.Arg(0)

	view, code := buildSkillView(env, skill)
	if code != ExitOK {
		// On error buildSkillView returns view=nil; do not defer zero().
		return code
	}
	defer view.zero()

	if *jsonOut {
		return writeShowJSON(env.Stdout, view)
	}
	return writeShowText(env.Stdout, view)
}

// runConfigGet prints a single config field's resolved value. Useful
// for shell scripts: `port=$(omac config get tng-email TNG_EMAIL_IMAP_PORT)`.
//
// Intentionally does NOT support fetching secrets — exposing a secret
// over stdout (and the user's shell history) defeats the keychain.
// Use `omac config show --json` to see fingerprints.
func runConfigGet(args []string, env *Env) int {
	if len(args) != 2 {
		fmt.Fprintln(env.Stderr, "Usage: omac config get <skill> <field>")
		return ExitMisuse
	}
	skill, field := args[0], args[1]

	view, code := buildSkillView(env, skill)
	if code != ExitOK {
		return code
	}
	defer view.zero()

	for _, f := range view.Config {
		if f.Name != field {
			continue
		}
		// Suppress placeholder strings ("<missing-required>" etc.) so
		// $(...) substitutions don't accidentally grab a marker.
		switch f.Source {
		case "missing-required", "missing-optional":
			fmt.Fprintf(env.Stderr, "omac config get: %s/%s is not set\n", skill, field)
			return ExitConfigInvalid
		}
		fmt.Fprintln(env.Stdout, f.Value)
		return ExitOK
	}
	// Don't leak that secrets exist by name.
	fmt.Fprintf(env.Stderr, "omac config get: %q has no config field named %q (use 'omac config show %s' for available fields)\n", skill, field, skill)
	return ExitConfigInvalid
}

// fieldView is one row in the config-show output. Field tags are JSON
// because the --json branch marshals these directly.
type fieldView struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Value       string `json:"value"`
	Source      string `json:"source"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

// secretView is the secrets equivalent. Value is always a fingerprint
// or "<missing>"; the plaintext never appears in this struct.
type secretView struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	// Source names where the value came from — "keychain",
	// "env_passthrough", or a missing-* marker — mirroring the config
	// table's column. Without it a secret satisfied from the host env is
	// indistinguishable from one stored in the keychain, and `omac start`
	// accepts both.
	Source      string `json:"source"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

// skillView is the per-skill bundle returned by buildSkillView. Carries
// everything both --json and the text formatter need.
type skillView struct {
	Skill   string       `json:"skill"`
	Mount   string       `json:"mount"`
	Workdir string       `json:"workdir"`
	Config  []fieldView  `json:"config"`
	Secrets []secretView `json:"secrets"`

	// owned holds Secret values whose plaintext is briefly in memory
	// for fingerprint computation; zero() wipes them.
	owned []secrets.Secret
}

func (v *skillView) zero() {
	for i := range v.owned {
		v.owned[i].Zero()
	}
}

// buildSkillView is the shared work both `show` and `get` need. On
// failure it prints to env.Stderr and returns a non-ExitOK code so
// callers can return it verbatim.
func buildSkillView(env *Env, skill string) (*skillView, int) {
	meta, _, err := loadRegisteredMeta(env, skill)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac config:", err)
		return nil, ExitPrerequisiteMissing
	}
	if meta.Sidecar == nil {
		// Should never happen: register rejects metas without sidecar
		// blocks. Fail loudly so the inconsistency is visible.
		fmt.Fprintf(env.Stderr, "omac config: skill %q has no sidecar block in %s\n", skill, config.MetaFileName)
		return nil, ExitConfigInvalid
	}

	workdirStore, err := skillconfig.Load(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac config: skill-config:", err)
		return nil, ExitIOError
	}
	globalStore, err := skillconfig.LoadGlobal()
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac config: global skill-config:", err)
		return nil, ExitIOError
	}
	// Merge so a globally-registered skill's stored values surface
	// here exactly as `omac start` would resolve them (workdir wins).
	store := skillstate.MergeConfig(globalStore, workdirStore)

	// Resolve through the shared rule so what this command SHOWS is by
	// construction what start/serve would USE. Two divergences disappear with
	// it: secrets were read unscoped here while start reads them workdir-scoped
	// with an unscoped fallback, and a secret satisfied via env_passthrough was
	// displayed as "<missing>" even though the sidecar receives it.
	//
	// SkipBundleHash: this command reports values, not drift.
	//
	// Scope: workdir-scoped with the unscoped fallback inside GetWithFallback,
	// matching start — which uses the same scope for globally-registered skills
	// too, since their secrets live under the unscoped key the fallback finds.
	armed, problems := skillstate.New(skillstate.Options{
		Scope:          keychain.WorkdirID(env.Workdir),
		SkipBundleHash: true,
	}).Resolve(meta, registry.Entry{Name: skill}, "", store)
	// The fingerprints below need the plaintext briefly; hand ownership to the
	// view so its zero() wipes it alongside everything else.
	out := &skillView{
		Skill:   skill,
		Mount:   armed.Mount,
		Workdir: env.Workdir,
		owned:   []secrets.Secret{},
	}
	for name := range armed.Secrets {
		out.owned = append(out.owned, armed.Secrets[name])
	}

	// A keychain that cannot answer is not "this secret is unset", so say so on
	// stderr — but keep going and print the table. This command's whole job is
	// to show what start WOULD inject, and every config field plus every
	// env_passthrough-supplied secret is still knowable without a keychain.
	// Aborting would also break `omac config get <skill> <field>` for plain
	// non-secret fields — the documented shell-substitution use case — on every
	// headless host.
	if p := skillstate.First(problems, skillstate.KeychainUnavailable); p != nil {
		detail := p.Detail
		if p.Fix != "" {
			detail += " — " + p.Fix
		}
		fmt.Fprintln(env.Stderr, "omac config: warning: keychain:", detail)
	}

	for _, spec := range meta.Sidecar.Config {
		out.Config = append(out.Config, fieldView{
			Name:        spec.Name,
			Type:        string(spec.EffectiveType()),
			Required:    spec.IsRequired(),
			Description: spec.Description,
			Value:       displayedConfigValue(armed, spec.Name),
			Source:      string(armed.ConfigSources[spec.Name]),
		})
	}

	for _, spec := range meta.Sidecar.Secrets {
		view := secretView{
			Name:        spec.Name,
			Required:    spec.IsRequired(),
			Description: spec.Description,
			Source:      string(armed.SecretSources[spec.Name]),
			Fingerprint: "<missing>",
		}
		if val, ok := armed.Secrets[spec.Name]; ok {
			view.Fingerprint = secretFingerprint(val.ExposeString())
		}
		out.Secrets = append(out.Secrets, view)
	}

	return out, ExitOK
}

// displayedConfigValue renders a config field for display, substituting the
// placeholder markers for an unresolved one. `omac config get` switches on the
// matching Source so a $(...) substitution never captures a marker.
func displayedConfigValue(armed skillstate.Armed, field string) string {
	if v, ok := armed.Config[field]; ok {
		return v
	}
	if armed.ConfigSources[field] == skillstate.SourceMissingOpt {
		return "<missing-optional>"
	}
	return "<missing-required>"
}

// secretFingerprint returns sha256(s)[:12] in hex, with a "sha256:"
// prefix to match the reference echo-rest sidecar's /whoami output
// byte-for-byte. Empty input yields the literal "<absent>" so the
// caller can tell "secret is the empty string" from "secret was never
// set" — but in practice keychain.Get returns ErrNotFound for the
// latter, which is handled upstream.
func secretFingerprint(s string) string {
	if s == "" {
		return "<absent>"
	}
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// writeShowText emits the human-readable view via tabwriter for clean
// column alignment regardless of value length.
func writeShowText(w io.Writer, v *skillView) int {
	fmt.Fprintf(w, "skill:   %s\n", v.Skill)
	fmt.Fprintf(w, "mount:   /%s/\n", v.Mount)
	fmt.Fprintf(w, "workdir: %s\n", v.Workdir)

	if len(v.Config) > 0 {
		fmt.Fprintln(w, "\nconfig:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tTYPE\tREQ\tSOURCE\tVALUE")
		for _, f := range v.Config {
			req := "no"
			if f.Required {
				req = "yes"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", f.Name, f.Type, req, f.Source, displayValue(f))
		}
		_ = tw.Flush()
	} else {
		fmt.Fprintln(w, "\nconfig: (none declared)")
	}

	if len(v.Secrets) > 0 {
		fmt.Fprintln(w, "\nsecrets:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tREQ\tSOURCE\tFINGERPRINT")
		for _, s := range v.Secrets {
			req := "no"
			if s.Required {
				req = "yes"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", s.Name, req, s.Source, s.Fingerprint)
		}
		_ = tw.Flush()
	} else {
		fmt.Fprintln(w, "\nsecrets: (none declared)")
	}
	return ExitOK
}

// displayValue truncates very long config values so a stray multi-KiB
// string in skill-config.yaml doesn't ruin terminal output. The full
// value is always available via --json.
func displayValue(f fieldView) string {
	const max = 80
	if len(f.Value) <= max {
		return f.Value
	}
	return f.Value[:max-1] + "…"
}

// writeShowJSON emits a stable JSON object on stdout. Stable key order
// is guaranteed by the struct field order; encoding/json walks fields
// declaration-first, not alphabetically.
func writeShowJSON(w io.Writer, v *skillView) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "omac config: json:", err)
		return ExitIOError
	}
	return ExitOK
}
