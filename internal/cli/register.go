package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillsource"
)

func runRegister(args []string, env *Env) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var (
		force           = fs.Bool("force", false, "Update registry entry even if meta_hash differs.")
		reprompt        = fs.Bool("reprompt-secrets", false, "Re-prompt for secrets even if already stored.")
		noSecrets       = fs.Bool("no-secrets", false, "Skip all secret prompts; caller promises to supply them at start time.")
		secretsFromPath = fs.String("secrets-from", "", "Read KEY=VALUE secrets from this file instead of prompting.")
		repromptFields  = fs.Bool("reprompt-fields", false, "Re-prompt for non-secret config fields even if already stored.")
		noFields        = fs.Bool("no-fields", false, "Skip all config-field prompts; caller promises to supply them at start time.")
		fieldsFromPath  = fs.String("fields-from", "", "Read KEY=VALUE config fields from this file instead of prompting.")
	)
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac register <skill> [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return ExitMisuse
	}
	skillName := fs.Arg(0)

	// Locate the skill in the workdir-local layer first, then fall
	// back to user-global. Within each layer, .agents/skills ranks
	// above the legacy .opencode/skills; see skillsource for the
	// full precedence list. The path that wins is recorded in the
	// registry; downstream code (start, config show) just uses
	// SkillDir verbatim, so the source layer is transparent after
	// registration.
	skillDir, src, err := skillsource.Resolve(env.Workdir, skillName)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac register:", err)
		if errors.Is(err, iofs.ErrNotExist) {
			return ExitPrerequisiteMissing
		}
		return ExitConfigInvalid
	}
	metaPath := filepath.Join(skillDir, config.MetaFileName)
	if src.Kind != "workdir" {
		fmt.Fprintf(env.Stdout, "[info] using user-global skill source: %s\n", skillDir)
	}

	// 1. Load + validate meta.
	meta, err := config.LoadMeta(metaPath)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac register:", err)
		if errors.Is(err, iofs.ErrNotExist) {
			return ExitPrerequisiteMissing
		}
		return ExitConfigInvalid
	}
	if meta.Sidecar == nil {
		fmt.Fprintf(env.Stderr, "omac register: skill %q has no sidecar block; nothing to register\n", skillName)
		return ExitMisuse
	}
	bundleHash, err := config.BundleHash(skillDir)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac register: bundle hash:", err)
		return ExitIOError
	}

	// 2. Secret handling.
	declaredNames := make([]string, 0, len(meta.Sidecar.Secrets))
	for _, s := range meta.Sidecar.Secrets {
		declaredNames = append(declaredNames, s.Name)
	}

	// User-global skills register once, globally; workdir-local skills
	// register per-workdir. `global` selects which set of registry /
	// skill-config locations and locks the rest of this function uses.
	// (Keychain secrets are keyed by skill name and are global either
	// way, so they are unaffected.)
	global := src.Kind == "user-global"

	// Pull the previous registration's "intentionally skipped" lists so
	// re-register doesn't re-prompt for optional values the user
	// already explicitly declined. This must happen before the prompt
	// loop runs.
	prevSkippedSecrets, prevSkippedFields := loadPrevSkipped(env.Workdir, skillName, global)

	// We rebuild these on every register run so the registry reflects
	// the user's most recent answers. A field that was skipped last
	// time but answered this time correctly drops out of the list.
	skippedSecrets := make([]string, 0)
	skippedFields := make([]string, 0)

	if !*noSecrets {
		fromFile, err := loadSecretsFile(*secretsFromPath)
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac register:", err)
			return ExitConfigInvalid
		}
		for _, spec := range meta.Sidecar.Secrets {
			skipped, err := handleOneSecret(env, skillName, spec, *reprompt, prevSkippedSecrets, fromFile)
			if err != nil {
				// Determine exit code from err message tag.
				if strings.HasPrefix(err.Error(), "keychain:") {
					fmt.Fprintln(env.Stderr, "omac register:", err)
					return ExitKeychainError
				}
				if strings.HasPrefix(err.Error(), "refused:") {
					fmt.Fprintln(env.Stderr, "omac register:", err)
					return ExitSecretRefused
				}
				fmt.Fprintln(env.Stderr, "omac register:", err)
				return ExitGeneric
			}
			if skipped {
				skippedSecrets = append(skippedSecrets, spec.Name)
			}
		}
	} else {
		// --no-secrets: the user opted out of secret handling for this
		// run. Preserve the previous skipped-list verbatim so a later
		// register without --no-secrets still benefits from the memory.
		for n := range prevSkippedSecrets {
			skippedSecrets = append(skippedSecrets, n)
		}
	}

	// 2b. Non-secret config fields. Stored in plain YAML under
	//     <workdir>/.opencode/skill-config.yaml for workdir-local
	//     skills, or ~/.config/omac/skill-config.yaml for user-global
	//     skills (NOT the keychain in either case).
	if !*noFields && len(meta.Sidecar.Config) > 0 {
		fromFile, err := loadFieldsFile(*fieldsFromPath)
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac register:", err)
			return ExitConfigInvalid
		}
		// Single load+save cycle so partial failures don't leave half a
		// skill's config behind. The flock further down is shared with
		// the registry update so the whole register operation is atomic
		// from another concurrent omac invocation's point of view.
		if err := withRegistryLock(env.Workdir, global, func() error {
			store, err := loadSkillConfig(env.Workdir, global)
			if err != nil {
				return err
			}
			for _, spec := range meta.Sidecar.Config {
				skipped, err := handleOneField(env, store, skillName, spec, *repromptFields, prevSkippedFields, fromFile)
				if err != nil {
					return err
				}
				if skipped {
					skippedFields = append(skippedFields, spec.Name)
				}
			}
			return saveSkillConfig(env.Workdir, global, store)
		}); err != nil {
			if strings.HasPrefix(err.Error(), "refused:") {
				fmt.Fprintln(env.Stderr, "omac register:", err)
				return ExitSecretRefused
			}
			fmt.Fprintln(env.Stderr, "omac register: skill-config:", err)
			return ExitIOError
		}
	} else if *noFields {
		// Symmetric to the --no-secrets branch above.
		for n := range prevSkippedFields {
			skippedFields = append(skippedFields, n)
		}
	}

	// 3. Install-script discovery. We surface the path so users know
	//    where to look and what to run, but we do NOT print the body
	//    of the script — the dump was historically useful when most
	//    skills shipped tiny one-liner scripts; modern skills often
	//    have hundreds of lines of brew/apt logic, and the noise hurt
	//    more than the discoverability helped. omac never runs the
	//    script for you; that's still entirely the user's call.
	host := osinfo.Detect()
	if scriptRel := meta.Sidecar.InstallScriptFor(host); scriptRel != "" {
		scriptAbs := filepath.Join(skillDir, scriptRel)
		if _, statErr := os.Stat(scriptAbs); statErr == nil {
			fmt.Fprintf(env.Stdout,
				"\n[info] install script for %s: %s\n"+
					"       omac does not run this; inspect it and run it yourself if needed:\n"+
					"         bash %s\n",
				host, scriptAbs, scriptAbs)
		} else if errors.Is(statErr, os.ErrNotExist) {
			// omac.yaml declares an install script but the file isn't
			// on disk. Surface this as a hint instead of a hard error
			// because the meta has already been validated and lots of
			// skills reference an install_scripts: entry without
			// shipping every OS variant.
			fmt.Fprintf(env.Stderr,
				"[warn] install script for %s declared in %s but missing on disk: %s\n",
				host, config.MetaFileName, scriptAbs)
		} else {
			fmt.Fprintln(env.Stderr, "omac register: stat install script:", statErr)
			return ExitIOError
		}
	}

	// 4. Registry update (atomic, under flock). Targets the global
	//    registry for user-global skills, the workdir registry
	//    otherwise.
	if err := withRegistryLock(env.Workdir, global, func() error {
		reg, err := loadRegistry(env.Workdir, global)
		if err != nil {
			return err
		}
		if existing, _ := reg.Find(skillName); existing != nil {
			if existing.BundleHash != bundleHash && !*force {
				return fmt.Errorf("already registered with a different bundle_hash; pass --force to update")
			}
		}
		// SkillDir is stored relative to the workdir for workdir-local
		// skills (so the registry stays portable when the project is
		// moved or shared) and absolute for everything else
		// (user-global, ad-hoc paths). Downstream code joins with
		// env.Workdir only when the stored path is not already absolute.
		stored := skillDir
		if src.Kind == "workdir" {
			stored = rel(env.Workdir, skillDir)
		}
		// Sort for stable on-disk diffs; nil out empty slices so the
		// omitempty JSON tag actually elides them.
		skippedSecretsOut := dedupSorted(skippedSecrets)
		skippedFieldsOut := dedupSorted(skippedFields)
		reg.Upsert(registry.Entry{
			Name:                skillName,
			SkillDir:            stored,
			BundleHash:          bundleHash,
			RegisteredAt:        time.Now().UTC(),
			DeclaredSecretNames: declaredNames,
			SkippedSecretNames:  skippedSecretsOut,
			SkippedConfigFields: skippedFieldsOut,
		})
		return saveRegistry(env.Workdir, global, reg)
	}); err != nil {
		fmt.Fprintln(env.Stderr, "omac register: registry:", err)
		return ExitIOError
	}

	if global {
		fmt.Fprintf(env.Stdout, "\n[ok] registered %s (global; available in every workdir)\n", skillName)
	} else {
		fmt.Fprintf(env.Stdout, "\n[ok] registered %s (workdir=%s)\n", skillName, env.Workdir)
	}
	return ExitOK
}

// loadRegistry / saveRegistry / withRegistryLock and the skillconfig
// equivalents route to either the workdir-scoped or the user-global
// store depending on `global`. They keep the per-source branching in
// one place so register/start/deregister can stay readable.
func loadRegistry(workdir string, global bool) (*registry.Registry, error) {
	if global {
		return registry.LoadGlobal()
	}
	return registry.Load(workdir)
}

func saveRegistry(workdir string, global bool, reg *registry.Registry) error {
	if global {
		return registry.SaveGlobal(reg)
	}
	return registry.Save(workdir, reg)
}

func withRegistryLock(workdir string, global bool, fn func() error) error {
	if global {
		return registry.WithGlobalLock(fn)
	}
	return registry.WithLock(workdir, fn)
}

func loadSkillConfig(workdir string, global bool) (*skillconfig.Store, error) {
	if global {
		return skillconfig.LoadGlobal()
	}
	return skillconfig.Load(workdir)
}

func saveSkillConfig(workdir string, global bool, store *skillconfig.Store) error {
	if global {
		return skillconfig.SaveGlobal(store)
	}
	return skillconfig.Save(workdir, store)
}

// rel returns path relative to base, or the original if not reachable.
func rel(base, path string) string {
	r, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return r
}

// loadPrevSkipped looks up the previously-recorded skip lists for
// skill in the workdir's registry. Both maps are non-nil even when
// the skill is unregistered or has no prior skips, so call sites can
// always do `prev[name]` without a nil check.
//
// Failures (registry parse errors, missing file, etc.) are swallowed
// and treated as "no prior skips" — re-prompting too much on a corrupt
// registry is the safer failure mode than silently honoring stale
// skips, and the registry update step further down will surface the
// real error if there is one.
func loadPrevSkipped(workdir, skill string, global bool) (secrets, fields map[string]bool) {
	secrets = map[string]bool{}
	fields = map[string]bool{}
	reg, err := loadRegistry(workdir, global)
	if err != nil || reg == nil {
		return
	}
	e, _ := reg.Find(skill)
	if e == nil {
		return
	}
	for _, n := range e.SkippedSecretNames {
		secrets[n] = true
	}
	for _, n := range e.SkippedConfigFields {
		fields[n] = true
	}
	return
}

// dedupSorted returns a stable, deduplicated, sorted copy of names, or
// nil for an empty input so JSON `omitempty` actually elides it.
func dedupSorted(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// handleOneSecret implements the per-secret flow from §16.4.
//
// The bool return reports whether the user explicitly skipped an
// optional secret on this run (entered an empty line, no
// default_from_env, required: false). Callers persist the names of
// skipped optional secrets in the registry so a later
// `omac register --force <skill>` doesn't pester the user about the
// same optional value over and over. `--reprompt-secrets` ignores the
// memory and resets it.
//
// The "already in keychain" branch and the "from --secrets-from / env"
// branches all return skipped=false: they record actual values, which
// take priority over any previous skip.
func handleOneSecret(env *Env, skill string, spec config.SecretSpec, reprompt bool, prevSkipped map[string]bool, fromFile map[string]string) (bool, error) {
	// 1. Already in keychain?
	if !reprompt {
		present, err := keychain.Has(skill, spec.Name)
		if err != nil {
			return false, fmt.Errorf("keychain: %w", err)
		}
		if present {
			fmt.Fprintf(env.Stderr, "  [skip] %s already in keychain\n", spec.Name)
			return false, nil
		}
		// 1b. Previously skipped on a prior register run? Honor that
		//     unless --reprompt-secrets is set. This is the fix for
		//     "register --force re-asks every optional secret".
		if prevSkipped[spec.Name] && !spec.IsRequired() {
			fmt.Fprintf(env.Stderr, "  [skip] %s (optional, previously declined)\n", spec.Name)
			return true, nil
		}
	}

	// 2. --secrets-from file takes precedence over prompting.
	if v, ok := fromFile[spec.Name]; ok {
		if err := validatePattern(spec, v); err != nil {
			return false, err
		}
		s := secrets.NewSecretString(v)
		defer s.Zero()
		if err := keychain.Set(skill, spec.Name, s); err != nil {
			return false, fmt.Errorf("keychain: %w", err)
		}
		fmt.Fprintf(env.Stderr, "  stored %s (from file)\n", spec.Name)
		return false, nil
	}

	// 3. Env-based non-interactive supply: OMAC_SECRET_<NAME>.
	if v, ok := os.LookupEnv("OMAC_SECRET_" + spec.Name); ok {
		if err := validatePattern(spec, v); err != nil {
			return false, err
		}
		s := secrets.NewSecretString(v)
		defer s.Zero()
		if err := keychain.Set(skill, spec.Name, s); err != nil {
			return false, fmt.Errorf("keychain: %w", err)
		}
		fmt.Fprintf(env.Stderr, "  stored %s (from OMAC_SECRET_%s)\n", spec.Name, spec.Name)
		return false, nil
	}

	// 4. default_from_env offers a pre-filled default on the prompt.
	var defaultHint string
	if spec.DefaultFromEnv != "" {
		if v, ok := os.LookupEnv(spec.DefaultFromEnv); ok && v != "" {
			// Accept on empty input.
			if err := validatePattern(spec, v); err == nil {
				defaultHint = fmt.Sprintf(" (press Enter to accept value from $%s)", spec.DefaultFromEnv)
			}
		}
	}

	// 5. Interactive prompt loop.
	if spec.Description != "" {
		fmt.Fprintf(env.Stderr, "  %s: %s\n", spec.Name, spec.Description)
	}
	attempts := 0
	for {
		attempts++
		prompt := fmt.Sprintf("  enter %s%s: ", spec.Name, defaultHint)
		value, err := secrets.ReadPassword(prompt)
		if err != nil {
			return false, fmt.Errorf("read %s: %w", spec.Name, err)
		}
		// Empty input → accept default_from_env if offered, else treat per `required`.
		if value.IsEmpty() {
			value.Zero()
			if defaultHint != "" {
				if v, ok := os.LookupEnv(spec.DefaultFromEnv); ok && v != "" {
					s := secrets.NewSecretString(v)
					if err := keychain.Set(skill, spec.Name, s); err != nil {
						s.Zero()
						return false, fmt.Errorf("keychain: %w", err)
					}
					s.Zero()
					fmt.Fprintf(env.Stderr, "  stored %s (from $%s)\n", spec.Name, spec.DefaultFromEnv)
					return false, nil
				}
			}
			if !spec.IsRequired() {
				fmt.Fprintf(env.Stderr, "  [skip] %s (optional, not provided)\n", spec.Name)
				return true, nil
			}
			if attempts >= 3 {
				return false, fmt.Errorf("refused: required secret %q not supplied", spec.Name)
			}
			fmt.Fprintln(env.Stderr, "  [retry] required; please enter a value")
			continue
		}
		if err := validatePattern(spec, value.ExposeString()); err != nil {
			value.Zero()
			if attempts >= 3 {
				return false, fmt.Errorf("refused: %s does not match pattern after %d attempts", spec.Name, attempts)
			}
			fmt.Fprintf(env.Stderr, "  [retry] %s\n", err)
			continue
		}
		if err := keychain.Set(skill, spec.Name, value); err != nil {
			value.Zero()
			return false, fmt.Errorf("keychain: %w", err)
		}
		value.Zero()
		fmt.Fprintf(env.Stderr, "  stored %s\n", spec.Name)
		return false, nil
	}
}

func validatePattern(spec config.SecretSpec, v string) error {
	if spec.Pattern == "" {
		return nil
	}
	re, err := regexp.Compile(spec.Pattern)
	if err != nil {
		return fmt.Errorf("invalid pattern for %s: %w", spec.Name, err)
	}
	if !re.MatchString(v) {
		return fmt.Errorf("value for %s does not match /%s/", spec.Name, spec.Pattern)
	}
	return nil
}

// handleOneField implements the per-field prompting flow. Mirrors
// handleOneSecret but writes to the skillconfig store instead of the
// keychain, and supports typed inputs (string/bool/int/enum).
//
// The bool return reports whether the user explicitly skipped an
// optional field on this run (entered an empty line, no default,
// required: false). Symmetric to handleOneSecret. See its doc comment.
//
// Errors with the prefix "refused:" map to ExitSecretRefused at the
// caller level (we reuse that exit code because the semantics — user
// explicitly didn't supply a required value — are the same).
func handleOneField(env *Env, store *skillconfig.Store, skill string, spec config.ConfigSpec, reprompt bool, prevSkipped map[string]bool, fromFile map[string]string) (bool, error) {
	// 1. Already stored?
	if !reprompt {
		if _, ok := store.Get(skill, spec.Name); ok {
			fmt.Fprintf(env.Stderr, "  [skip] %s already configured\n", spec.Name)
			return false, nil
		}
		// 1b. Previously skipped on a prior register run? Honor that
		//     unless --reprompt-fields is set. Without this branch
		//     `omac register --force <skill>` re-asks every optional
		//     field on every run, which the user reasonably treats as
		//     a regression.
		if prevSkipped[spec.Name] && !spec.IsRequired() {
			fmt.Fprintf(env.Stderr, "  [skip] %s (optional, previously declined)\n", spec.Name)
			return true, nil
		}
	}

	// 2. --fields-from file beats prompting.
	if v, ok := fromFile[spec.Name]; ok {
		canon, err := canonicalizeFieldValue(spec, v)
		if err != nil {
			return false, err
		}
		store.Set(skill, spec.Name, canon)
		fmt.Fprintf(env.Stderr, "  set %s = %s (from file)\n", spec.Name, canon)
		return false, nil
	}

	// 3. Env-based non-interactive supply: OMAC_CONFIG_<NAME>.
	//    Distinct from OMAC_SECRET_<NAME> so a misuse can't accidentally
	//    leak a secret into the world-readable skill-config.yaml.
	if v, ok := os.LookupEnv("OMAC_CONFIG_" + spec.Name); ok {
		canon, err := canonicalizeFieldValue(spec, v)
		if err != nil {
			return false, err
		}
		store.Set(skill, spec.Name, canon)
		fmt.Fprintf(env.Stderr, "  set %s = %s (from $OMAC_CONFIG_%s)\n", spec.Name, canon, spec.Name)
		return false, nil
	}

	// 4. Pre-computed default for the prompt: explicit `default:`,
	//    or `default_from_env: <VAR>` if set in the host env.
	defaultVal := spec.Default
	defaultSource := "spec.default"
	if defaultVal == "" && spec.DefaultFromEnv != "" {
		if v, ok := os.LookupEnv(spec.DefaultFromEnv); ok && v != "" {
			defaultVal = v
			defaultSource = "$" + spec.DefaultFromEnv
		}
	}

	// 5. Interactive prompt loop.
	if spec.Description != "" {
		fmt.Fprintf(env.Stderr, "  %s: %s\n", spec.Name, spec.Description)
	}
	if spec.EffectiveType() == config.ConfigFieldEnum {
		fmt.Fprintf(env.Stderr, "    choices: %s\n", strings.Join(spec.Choices, ", "))
	}

	reader := bufio.NewReader(env.Stdin)
	attempts := 0
	for {
		attempts++
		var hint string
		if defaultVal != "" {
			hint = fmt.Sprintf(" [%s, from %s]", defaultVal, defaultSource)
		}
		fmt.Fprintf(env.Stderr, "  enter %s%s: ", spec.Name, hint)

		line, err := reader.ReadString('\n')
		if err != nil {
			// EOF on stdin (non-tty / piped). Treat as empty input
			// for the same default/required logic as a tty.
			line = ""
		}
		line = strings.TrimRight(line, "\r\n")

		if line == "" {
			if defaultVal != "" {
				canon, err := canonicalizeFieldValue(spec, defaultVal)
				if err != nil {
					// Default itself is invalid (e.g. enum default not in choices,
					// caught at meta validation but we double-check here). Don't
					// silently store garbage.
					return false, fmt.Errorf("default for %s rejected: %w", spec.Name, err)
				}
				store.Set(skill, spec.Name, canon)
				fmt.Fprintf(env.Stderr, "  set %s = %s (default from %s)\n", spec.Name, canon, defaultSource)
				return false, nil
			}
			if !spec.IsRequired() {
				fmt.Fprintf(env.Stderr, "  [skip] %s (optional, not provided)\n", spec.Name)
				return true, nil
			}
			if attempts >= 3 {
				return false, fmt.Errorf("refused: required config field %q not supplied", spec.Name)
			}
			fmt.Fprintln(env.Stderr, "  [retry] required; please enter a value")
			continue
		}

		canon, err := canonicalizeFieldValue(spec, line)
		if err != nil {
			if attempts >= 3 {
				return false, fmt.Errorf("refused: %s rejected after %d attempts: %w", spec.Name, attempts, err)
			}
			fmt.Fprintf(env.Stderr, "  [retry] %s\n", err)
			continue
		}
		store.Set(skill, spec.Name, canon)
		fmt.Fprintf(env.Stderr, "  set %s = %s\n", spec.Name, canon)
		return false, nil
	}
}

// canonicalizeFieldValue type-checks a raw input string against the
// field spec and returns the canonical string form to be stored.
//
// For type=string, validates against the optional regex.
// For type=bool, accepts the spellings handled by config.ParseBoolField
//
//	and stores either "true" or "false".
//
// For type=int, requires a base-10 64-bit integer and stores the
//
//	strconv.FormatInt rendering.
//
// For type=enum, requires exact match against one of the choices.
func canonicalizeFieldValue(spec config.ConfigSpec, raw string) (string, error) {
	switch spec.EffectiveType() {
	case config.ConfigFieldString:
		if spec.Pattern != "" {
			re, err := regexp.Compile(spec.Pattern)
			if err != nil {
				return "", fmt.Errorf("invalid pattern for %s: %w", spec.Name, err)
			}
			if !re.MatchString(raw) {
				return "", fmt.Errorf("value for %s does not match /%s/", spec.Name, spec.Pattern)
			}
		}
		return raw, nil
	case config.ConfigFieldBool:
		v, err := config.ParseBoolField(raw)
		if err != nil {
			return "", fmt.Errorf("value for %s: %w", spec.Name, err)
		}
		return v, nil
	case config.ConfigFieldInt:
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return "", fmt.Errorf("value for %s: not a valid integer: %q", spec.Name, raw)
		}
		return strconv.FormatInt(n, 10), nil
	case config.ConfigFieldEnum:
		for _, choice := range spec.Choices {
			if choice == raw {
				return raw, nil
			}
		}
		return "", fmt.Errorf("value for %s must be one of: %s", spec.Name, strings.Join(spec.Choices, ", "))
	default:
		return "", fmt.Errorf("internal: unknown field type %q", spec.Type)
	}
}

// loadFieldsFile reads KEY=VALUE config-field lines. Same wire format
// as loadSecretsFile (which it intentionally duplicates rather than
// shares, in case the two formats diverge in future — e.g. fields may
// gain support for nested values).
func loadFieldsFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open --fields-from: %w", err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("--fields-from: missing '=' in line: %s", line)
		}
		out[line[:eq]] = line[eq+1:]
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read --fields-from: %w", err)
	}
	return out, nil
}

// loadSecretsFile reads KEY=VALUE lines. Empty lines and # comments are skipped.
func loadSecretsFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open --secrets-from: %w", err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("--secrets-from: missing '=' in line: %s", line)
		}
		key, val := line[:eq], line[eq+1:]
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read --secrets-from: %w", err)
	}
	return out, nil
}
