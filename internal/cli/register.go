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
		useDefaults     = fs.Bool("defaults", false, "Non-interactive: use remembered global defaults where available; prompt only for values never set anywhere.")
		harnessName     = fs.String("harness", "", "Resolve the skill in this harness's scope (opencode|claude). Default: opencode. Use to disambiguate a skill name present under multiple harnesses.")
		globalOnly      = fs.Bool("global", false, "When a skill name exists both workdir-local and user-global, register the user-global one.")
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

	// Locate the skill, honoring harness scope and disambiguation. Discovery
	// is harness-scoped: each harness sees only its own skills dir plus the
	// shared .agents dir. A skill name can be ambiguous across harnesses
	// (same name under .opencode/skills and .claude/skills) or across scope
	// (workdir vs user-global). We detect both and ask the user to pick with
	// --harness / --global rather than silently guessing.
	skillDir, src, regHarness, code := resolveRegisterTarget(env, skillName, *harnessName, *globalOnly)
	if code != ExitOK {
		return code
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
	global := src.Kind == "user-global"

	// Keychain scope for this skill's secrets. Global skills use the
	// unscoped key (omac/<skill>); workdir-local skills use the
	// workdir-scoped key (omac/<workdir-id>/<skill>) so two projects'
	// same-named skills don't share a credential. This MUST match the
	// scope serve reads with (serve.go bringUp), or the secret is invisible
	// to the running sidecar. See docs/MULTI_DIR_DESKTOP.md §4.3.
	secretScope := ""
	if !global {
		secretScope = keychain.WorkdirID(env.Workdir)
	}

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

	// Styler for the human-facing status lines and prompts (these go to
	// stderr); a separate one drives stdout's final summary + callout.
	sErr := newStyler(env.Stderr)

	if !*noSecrets {
		if len(meta.Sidecar.Secrets) > 0 {
			sErr.heading(env.Stderr, "Secrets")
		}
		fromFile, err := loadSecretsFile(*secretsFromPath)
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac register:", err)
			return ExitConfigInvalid
		}
		for _, spec := range meta.Sidecar.Secrets {
			skipped, err := handleOneSecret(env, secretScope, skillName, spec, *reprompt, prevSkippedSecrets, fromFile, *useDefaults)
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
		sErr.heading(env.Stderr, "Config fields")
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
			// For --defaults, load the global store so we can read the
			// remembered config defaults and mirror new values back into
			// them. When registering a global skill, store IS the global
			// store, so reuse it.
			// The defaults store is ALWAYS populated (not just under
			// --defaults), so the first register remembers config values for
			// later `register --defaults` reuse. --defaults only controls the
			// read/adopt side inside handleOneField. For a global skill the
			// global skill-config IS the defaults store.
			var defStore *skillconfig.Store
			if global {
				defStore = store
			} else {
				gs, err := skillconfig.LoadGlobal()
				if err != nil {
					return err
				}
				defStore = gs
			}
			for _, spec := range meta.Sidecar.Config {
				skipped, err := handleOneField(env, store, skillName, spec, *repromptFields, prevSkippedFields, fromFile, *useDefaults, defStore)
				if err != nil {
					return err
				}
				if skipped {
					skippedFields = append(skippedFields, spec.Name)
				}
			}
			if err := saveSkillConfig(env.Workdir, global, store); err != nil {
				return err
			}
			// Persist mirrored config defaults when they live in a
			// separate (global) store. For a global skill, defStore ==
			// store and was already saved above.
			if !global && defStore != nil {
				if err := registry.WithGlobalLock(func() error {
					return saveSkillConfig(env.Workdir, true, defStore)
				}); err != nil {
					return err
				}
			}
			return nil
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

	// 3. Install-script discovery. We resolve the path here (so a
	//    missing-on-disk declaration still fails fast / warns early)
	//    but defer surfacing it until the very end, where it gets a
	//    prominent boxed callout — running the install script is the
	//    most common "what next?" after a register, so it shouldn't be
	//    buried mid-output. We never print the body of the script, and
	//    omac never runs it for you; that's entirely the user's call.
	host := osinfo.Detect()
	var installScriptAbs string
	if scriptRel := meta.Sidecar.InstallScriptFor(host); scriptRel != "" {
		scriptAbs := filepath.Join(skillDir, scriptRel)
		if _, statErr := os.Stat(scriptAbs); statErr == nil {
			installScriptAbs = scriptAbs
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
		// Scope the conflict check to the harness we're registering under.
		// The registry keys entries by (Name, Harness) and the same skill
		// may legitimately be registered under multiple harnesses (each with
		// its own on-disk copy and bundle hash). Using the name-only Find
		// here would compare against another harness's entry and reject a
		// genuinely new-harness registration as a false bundle_hash conflict.
		if existing, _ := reg.FindForHarness(skillName, regHarness); existing != nil {
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
			Harness:             regHarness,
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

	sOut := newStyler(env.Stdout)
	okTag := sOut.paint("[ok]", ansiBold, ansiGreen)
	if global {
		fmt.Fprintf(env.Stdout, "\n%s registered %s %s\n",
			okTag, sOut.bold(skillName), sOut.gray("(global; available in every workdir)"))
		// Ask a running omac serve to re-activate the global layer so the
		// newly-registered global skill is mounted without a restart.
		if ok, msg := notifyReloadGlobal(); ok {
			fmt.Fprintf(env.Stdout, "%s %s\n", okTag, msg)
		} else if msg != "" && msg != "no running omac serve detected" {
			fmt.Fprintf(env.Stdout, "%s %s\n", sOut.paint("[info]", ansiCyan), msg)
		}
	} else {
		fmt.Fprintf(env.Stdout, "\n%s registered %s %s\n",
			okTag, sOut.bold(skillName), sOut.gray("(workdir="+env.Workdir+")"))
		// If an omac serve is running, ask it to reload this directory so the
		// newly-registered skill is picked up without a restart.
		if ok, msg := notifyReload(env.Workdir); ok {
			fmt.Fprintf(env.Stdout, "%s %s\n", okTag, msg)
		} else if msg != "" && msg != "no running omac serve detected" {
			fmt.Fprintf(env.Stdout, "%s %s\n", sOut.paint("[info]", ansiCyan), msg)
		}
	}

	// Prominent "what next?" callout for the install script. This is the
	// single most actionable follow-up after a register, so it gets a
	// bordered box at the very end where it can't be missed. omac never
	// runs the script itself.
	if installScriptAbs != "" {
		sOut.callout(env.Stdout, ansiYellow,
			fmt.Sprintf("NEXT STEP — install dependencies (%s)", host),
			[]string{
				sOut.dim("omac does not run this for you. Inspect it, then run:"),
				"",
				"    " + sOut.paint("bash "+installScriptAbs, ansiBold, ansiGreen),
			})
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
// handleOneSecret stores a secret under the given keychain scope. scope is
// "" for global skills (unscoped omac/<skill>) and the workdir-id for
// workdir-local skills (omac/<workdir-id>/<skill>) — it MUST match the scope
// serve reads with (see serve.go bringUp), or the skill will never see the
// value. See docs/MULTI_DIR_DESKTOP.md §4.3.
func handleOneSecret(env *Env, scope, skill string, spec config.SecretSpec, reprompt bool, prevSkipped map[string]bool, fromFile map[string]string, useDefaults bool) (bool, error) {
	st := newStyler(env.Stderr)
	// 1. Already in keychain?
	if !reprompt {
		present, err := keychain.HasScoped(scope, skill, spec.Name)
		if err != nil {
			return false, fmt.Errorf("keychain: %w", err)
		}
		if present {
			// Backfill the remembered-default mirror from the already-stored
			// value, so a secret stored before the defaults feature (or via a
			// path that didn't mirror) still becomes reusable with
			// `register --defaults`. Best-effort; never fails the skip.
			if cur, gerr := keychain.GetScoped(scope, skill, spec.Name); gerr == nil {
				_ = keychain.SetScopedDefaultMirror(skill, spec.Name, cur)
				cur.Zero()
			}
			st.status(env.Stderr, "[skip]", st.dim(spec.Name+" already in keychain"), ansiYellow)
			return false, nil
		}
		// 1b. Previously skipped on a prior register run? Honor that
		//     unless --reprompt-secrets is set. This is the fix for
		//     "register --force re-asks every optional secret".
		if prevSkipped[spec.Name] && !spec.IsRequired() {
			st.status(env.Stderr, "[skip]", st.dim(spec.Name+" (optional, previously declined)"), ansiYellow)
			return true, nil
		}
	}

	// 1c. --defaults: adopt the remembered global default silently if one
	//     exists (docs/MULTI_DIR_DESKTOP.md §4.4). If none exists we fall
	//     through and still prompt — --defaults means "don't re-ask for
	//     things I've already answered", not "skip required values".
	if useDefaults {
		if def, err := keychain.GetDefault(skill, spec.Name); err == nil {
			if verr := validatePattern(spec, def.ExposeString()); verr == nil {
				if serr := keychain.SetWithDefault(scope, skill, spec.Name, def); serr != nil {
					def.Zero()
					return false, fmt.Errorf("keychain: %w", serr)
				}
				def.Zero()
				st.status(env.Stderr, "stored", spec.Name+st.gray(" (from remembered default)"), ansiGreen)
				return false, nil
			}
			def.Zero()
		}
	}

	// 2. --secrets-from file takes precedence over prompting.
	if v, ok := fromFile[spec.Name]; ok {
		if err := validatePattern(spec, v); err != nil {
			return false, err
		}
		s := secrets.NewSecretString(v)
		defer s.Zero()
		if err := keychain.SetWithDefault(scope, skill, spec.Name, s); err != nil {
			return false, fmt.Errorf("keychain: %w", err)
		}
		st.status(env.Stderr, "stored", spec.Name+st.gray(" (from file)"), ansiGreen)
		return false, nil
	}

	// 3. Env-based non-interactive supply: OMAC_SECRET_<NAME>.
	if v, ok := os.LookupEnv("OMAC_SECRET_" + spec.Name); ok {
		if err := validatePattern(spec, v); err != nil {
			return false, err
		}
		s := secrets.NewSecretString(v)
		defer s.Zero()
		if err := keychain.SetWithDefault(scope, skill, spec.Name, s); err != nil {
			return false, fmt.Errorf("keychain: %w", err)
		}
		st.status(env.Stderr, "stored", spec.Name+st.gray(fmt.Sprintf(" (from OMAC_SECRET_%s)", spec.Name)), ansiGreen)
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
		fmt.Fprintf(env.Stderr, "  %s %s\n", st.bold(spec.Name), st.dim(spec.Description))
	}
	attempts := 0
	for {
		attempts++
		prompt := fmt.Sprintf("  %s %s%s ", st.cyan("?"), st.bold(spec.Name), st.gray(defaultHint+":"))
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
					if err := keychain.SetWithDefault(scope, skill, spec.Name, s); err != nil {
						s.Zero()
						return false, fmt.Errorf("keychain: %w", err)
					}
					s.Zero()
					st.status(env.Stderr, "stored", spec.Name+st.gray(fmt.Sprintf(" (from $%s)", spec.DefaultFromEnv)), ansiGreen)
					return false, nil
				}
			}
			if !spec.IsRequired() {
				st.status(env.Stderr, "[skip]", st.dim(spec.Name+" (optional, not provided)"), ansiYellow)
				return true, nil
			}
			if attempts >= 3 {
				return false, fmt.Errorf("refused: required secret %q not supplied", spec.Name)
			}
			st.status(env.Stderr, "[retry]", "required; please enter a value", ansiRed)
			continue
		}
		if err := validatePattern(spec, value.ExposeString()); err != nil {
			value.Zero()
			if attempts >= 3 {
				return false, fmt.Errorf("refused: %s does not match pattern after %d attempts", spec.Name, attempts)
			}
			st.status(env.Stderr, "[retry]", err.Error(), ansiRed)
			continue
		}
		if err := keychain.SetWithDefault(scope, skill, spec.Name, value); err != nil {
			value.Zero()
			return false, fmt.Errorf("keychain: %w", err)
		}
		value.Zero()
		st.status(env.Stderr, "stored", spec.Name, ansiGreen)
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
func handleOneField(env *Env, store *skillconfig.Store, skill string, spec config.ConfigSpec, reprompt bool, prevSkipped map[string]bool, fromFile map[string]string, useDefaults bool, defStore *skillconfig.Store) (bool, error) {
	st := newStyler(env.Stderr)
	// setField stores the value in the runtime store and, when defaults
	// are in play, mirrors it into the defaults store too.
	setField := func(val string) {
		store.Set(skill, spec.Name, val)
		if defStore != nil {
			defStore.SetDefault(skill, spec.Name, val)
		}
	}

	// 1. Already stored?
	if !reprompt {
		if cur, ok := store.Get(skill, spec.Name); ok {
			// Backfill the remembered default from the already-stored value
			// so `register --defaults` can reuse it later, even if the value
			// predates the defaults feature.
			if defStore != nil {
				defStore.SetDefault(skill, spec.Name, cur)
			}
			st.status(env.Stderr, "[skip]", st.dim(spec.Name+" already configured"), ansiYellow)
			return false, nil
		}
		// 1b. Previously skipped on a prior register run? Honor that
		//     unless --reprompt-fields is set. Without this branch
		//     `omac register --force <skill>` re-asks every optional
		//     field on every run, which the user reasonably treats as
		//     a regression.
		if prevSkipped[spec.Name] && !spec.IsRequired() {
			st.status(env.Stderr, "[skip]", st.dim(spec.Name+" (optional, previously declined)"), ansiYellow)
			return true, nil
		}
	}

	// 1c. --defaults: adopt the remembered global default silently if one
	//     exists and is valid (docs/MULTI_DIR_DESKTOP.md §4.4). Otherwise
	//     fall through and prompt.
	if useDefaults && defStore != nil {
		if def, ok := defStore.GetDefault(skill, spec.Name); ok {
			if canon, err := canonicalizeFieldValue(spec, def); err == nil {
				setField(canon)
				st.setLine(env.Stderr, spec.Name, canon, "(from remembered default)")
				return false, nil
			}
		}
	}

	// 2. --fields-from file beats prompting.
	if v, ok := fromFile[spec.Name]; ok {
		canon, err := canonicalizeFieldValue(spec, v)
		if err != nil {
			return false, err
		}
		setField(canon)
		st.setLine(env.Stderr, spec.Name, canon, "(from file)")
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
		setField(canon)
		st.setLine(env.Stderr, spec.Name, canon, fmt.Sprintf("(from $OMAC_CONFIG_%s)", spec.Name))
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
		fmt.Fprintf(env.Stderr, "  %s %s\n", st.bold(spec.Name), st.dim(spec.Description))
	}
	if spec.EffectiveType() == config.ConfigFieldEnum {
		fmt.Fprintf(env.Stderr, "    %s %s\n", st.gray("choices:"), st.cyan(strings.Join(spec.Choices, ", ")))
	}

	reader := bufio.NewReader(env.Stdin)
	attempts := 0
	for {
		attempts++
		var hint string
		if defaultVal != "" {
			hint = fmt.Sprintf(" [%s, from %s]", defaultVal, defaultSource)
		}
		fmt.Fprintf(env.Stderr, "  %s %s%s ", st.cyan("?"), st.bold(spec.Name), st.gray(hint+":"))

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
				setField(canon)
				st.setLine(env.Stderr, spec.Name, canon, fmt.Sprintf("(default from %s)", defaultSource))
				return false, nil
			}
			if !spec.IsRequired() {
				st.status(env.Stderr, "[skip]", st.dim(spec.Name+" (optional, not provided)"), ansiYellow)
				return true, nil
			}
			if attempts >= 3 {
				return false, fmt.Errorf("refused: required config field %q not supplied", spec.Name)
			}
			st.status(env.Stderr, "[retry]", "required; please enter a value", ansiRed)
			continue
		}

		canon, err := canonicalizeFieldValue(spec, line)
		if err != nil {
			if attempts >= 3 {
				return false, fmt.Errorf("refused: %s rejected after %d attempts: %w", spec.Name, attempts, err)
			}
			st.status(env.Stderr, "[retry]", err.Error(), ansiRed)
			continue
		}
		setField(canon)
		st.setLine(env.Stderr, spec.Name, canon, "")
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

// resolveRegisterTarget finds the single skill source dir to register,
// honoring harness scope (--harness) and scope (--global), and detecting
// ambiguity. On any error it prints a message and returns a non-OK exit code;
// on success it returns the winning skill dir, its source, the harness name to
// record on the registry entry ("" for a shared/.agents skill that is not
// specific to one harness), and ExitOK.
func resolveRegisterTarget(env *Env, skillName, harnessFlag string, globalOnly bool) (string, skillsource.Source, string, int) {
	// Gather candidates. If --harness is given, scope strictly to that
	// harness; otherwise scan every harness so we can detect a name that
	// exists under more than one harness.
	var (
		cands []skillsource.Candidate
		err   error
	)
	if harnessFlag != "" {
		h, ok := config.LookupHarness(harnessFlag)
		if !ok {
			fmt.Fprintln(env.Stderr, "omac register:", config.UnknownHarnessError(harnessFlag))
			return "", skillsource.Source{}, "", ExitMisuse
		}
		cands, err = skillsource.Candidates(env.Workdir, h, skillName)
	} else {
		cands, err = skillsource.CandidatesAllHarnesses(env.Workdir, skillName)
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac register:", err)
		return "", skillsource.Source{}, "", ExitIOError
	}
	if len(cands) == 0 {
		fmt.Fprintf(env.Stderr, "omac register: %q not found in any in-scope skill source\n", skillName)
		return "", skillsource.Source{}, "", ExitPrerequisiteMissing
	}

	// Apply --global scope filter (keep only user-global when asked).
	if globalOnly {
		filtered := cands[:0:0]
		for _, c := range cands {
			if c.Kind == "user-global" {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) == 0 {
			fmt.Fprintf(env.Stderr, "omac register: --global given but %q has no user-global source in scope\n", skillName)
			return "", skillsource.Source{}, "", ExitPrerequisiteMissing
		}
		cands = filtered
	}

	// Harness-level ambiguity: the name resolves under more than one harness
	// (excluding the shared layer, which is a single physical dir).
	if harnessFlag == "" && distinctHarnesses(cands) > 1 {
		printRegisterAmbiguity(env, skillName, cands, true)
		return "", skillsource.Source{}, "", ExitMisuse
	}

	// Down to a single harness scope. If still more than one candidate, it's
	// a scope ambiguity (workdir vs user-global). We surface it rather than
	// silently choosing, unless --global already narrowed it.
	if len(cands) > 1 {
		printRegisterAmbiguity(env, skillName, cands, false)
		return "", skillsource.Source{}, "", ExitMisuse
	}

	c := cands[0]
	// The harness recorded on the registry entry: empty for a shared/.agents
	// skill (it belongs to no single harness), else the candidate's harness.
	regHarness := c.Harness
	if regHarness == skillsource.SharedHarnessLabel {
		regHarness = ""
	}
	return c.Dir, skillsource.Source{Root: filepath.Dir(c.Dir), Kind: c.Kind}, regHarness, ExitOK
}

// distinctHarnesses counts how many distinct harness owners appear in the
// candidate set. The shared label collapses to a single non-harness bucket so
// a purely shared skill counts as one (not ambiguous).
func distinctHarnesses(cands []skillsource.Candidate) int {
	seen := map[string]struct{}{}
	for _, c := range cands {
		seen[c.Harness] = struct{}{}
	}
	return len(seen)
}

// printRegisterAmbiguity renders an aligned table of candidates and the exact
// flag(s) to disambiguate. harnessAmbiguous selects the hint (--harness vs
// --global).
func printRegisterAmbiguity(env *Env, skillName string, cands []skillsource.Candidate, harnessAmbiguous bool) {
	s := newStyler(env.Stderr)
	fmt.Fprintf(env.Stderr, "\n%s\n", s.bold(fmt.Sprintf("Multiple skills named %q found:", skillName)))

	// Compute column widths.
	hW, scW := len("HARNESS"), len("SCOPE")
	for _, c := range cands {
		if len(c.Harness) > hW {
			hW = len(c.Harness)
		}
		scope := scopeLabel(c.Kind)
		if len(scope) > scW {
			scW = len(scope)
		}
	}
	fmt.Fprintf(env.Stderr, "  %-*s  %-*s  %s\n", hW, "HARNESS", scW, "SCOPE", "PATH")
	for _, c := range cands {
		fmt.Fprintf(env.Stderr, "  %-*s  %-*s  %s\n", hW, c.Harness, scW, scopeLabel(c.Kind), c.Dir)
	}

	fmt.Fprintln(env.Stderr)
	if harnessAmbiguous {
		// Suggest a concrete --harness for the first non-shared candidate.
		example := skillName
		for _, c := range cands {
			if c.Harness != skillsource.SharedHarnessLabel {
				example = fmt.Sprintf("%s --harness %s", skillName, c.Harness)
				break
			}
		}
		fmt.Fprintf(env.Stderr, "Pick a harness:  %s\n", s.cyan("omac register "+example))
	} else {
		fmt.Fprintf(env.Stderr, "Pick the user-global one with %s, or omit it for the workdir-local one:\n", s.bold("--global"))
		fmt.Fprintf(env.Stderr, "  %s\n", s.cyan("omac register "+skillName+" --global"))
	}
}

func scopeLabel(kind string) string {
	if kind == "user-global" {
		return "global"
	}
	return "workdir"
}
