package cli

import (
	"flag"
	"fmt"
	"path/filepath"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
)

func runSecrets(args []string, env *Env) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "Usage: omac secrets <list|set|unset|import> <skill> [name] [flags]")
		return ExitMisuse
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runSecretsList(rest, env)
	case "set":
		return runSecretsSet(rest, env)
	case "unset":
		return runSecretsUnset(rest, env)
	case "import":
		return runSecretsImport(rest, env)
	default:
		fmt.Fprintf(env.Stderr, "omac secrets: unknown subcommand %q\n", sub)
		return ExitMisuse
	}
}

func runSecretsList(args []string, env *Env) int {
	if len(args) != 1 {
		fmt.Fprintln(env.Stderr, "Usage: omac secrets list <skill>")
		return ExitMisuse
	}
	skill := args[0]
	meta, global, err := loadRegisteredMeta(env, skill)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac secrets list:", err)
		return ExitPrerequisiteMissing
	}
	scope := secretScopeFor(env, global)
	if meta.Sidecar == nil || len(meta.Sidecar.Secrets) == 0 {
		fmt.Fprintln(env.Stdout, "(no secrets declared)")
		return ExitOK
	}
	for _, s := range meta.Sidecar.Secrets {
		present, err := keychain.HasScoped(scope, skill, s.Name)
		status := "absent"
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac secrets list:", err)
			return ExitKeychainError
		}
		if present {
			status = "present"
		}
		req := "optional"
		if s.IsRequired() {
			req = "required"
		}
		fmt.Fprintf(env.Stdout, "  %-28s %-8s %-8s %s\n", s.Name, req, status, s.Description)
	}
	return ExitOK
}

func runSecretsSet(args []string, env *Env) int {
	if len(args) != 2 {
		fmt.Fprintln(env.Stderr, "Usage: omac secrets set <skill> <name>")
		return ExitMisuse
	}
	skill, name := args[0], args[1]
	meta, global, err := loadRegisteredMeta(env, skill)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac secrets set:", err)
		return ExitPrerequisiteMissing
	}
	scope := secretScopeFor(env, global)
	spec, ok := findSecret(meta, name)
	if !ok {
		fmt.Fprintf(env.Stderr, "omac secrets set: %q does not declare a secret named %q\n", skill, name)
		return ExitConfigInvalid
	}
	// Always re-prompt. The prev-skipped map is irrelevant here because
	// reprompt=true bypasses both the keychain-cache check and the
	// previously-declined check inside handleOneSecret. The skipped
	// return value is also irrelevant: `omac secrets set` is a
	// pinpoint operation on a single secret and does not touch the
	// registry's skip list — that is solely owned by `omac register`.
	if _, err := handleOneSecret(env, scope, skill, spec, true, nil, nil, false); err != nil {
		fmt.Fprintln(env.Stderr, "omac secrets set:", err)
		return ExitKeychainError
	}
	// A supplied secret most often unblocks a pending-credentials skill. We
	// don't know here whether the skill is workdir-local or global, so ask a
	// running omac serve to reload both this directory and the global layer
	// (both best-effort; the irrelevant one is a harmless no-op).
	if ok, msg := notifyReload(env.Workdir); ok {
		fmt.Fprintf(env.Stdout, "[ok] %s\n", msg)
	}
	if ok, msg := notifyReloadGlobal(); ok {
		fmt.Fprintf(env.Stdout, "[ok] %s\n", msg)
	}
	return ExitOK
}

func runSecretsUnset(args []string, env *Env) int {
	if len(args) != 2 {
		fmt.Fprintln(env.Stderr, "Usage: omac secrets unset <skill> <name>")
		return ExitMisuse
	}
	skill, name := args[0], args[1]
	// Delete from both the workdir-scoped key and the unscoped/global key so
	// unset works regardless of how the secret was stored (missing entries
	// are not an error in keychain.DeleteScoped).
	scope := keychain.WorkdirID(env.Workdir)
	if err := keychain.DeleteScoped(scope, skill, name); err != nil {
		fmt.Fprintln(env.Stderr, "omac secrets unset:", err)
		return ExitKeychainError
	}
	if err := keychain.DeleteScoped("", skill, name); err != nil {
		fmt.Fprintln(env.Stderr, "omac secrets unset:", err)
		return ExitKeychainError
	}
	fmt.Fprintf(env.Stdout, "[ok] removed %s/%s from keychain\n", skill, name)
	return ExitOK
}

func runSecretsImport(args []string, env *Env) int {
	fs := flag.NewFlagSet("secrets import", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var file = fs.String("from", "", "KEY=VALUE file to import.")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac secrets import <skill> --from <file>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}
	if fs.NArg() != 1 || *file == "" {
		fs.Usage()
		return ExitMisuse
	}
	skill := fs.Arg(0)
	meta, global, err := loadRegisteredMeta(env, skill)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac secrets import:", err)
		return ExitPrerequisiteMissing
	}
	scope := secretScopeFor(env, global)
	declared := map[string]config.SecretSpec{}
	if meta.Sidecar != nil {
		for _, s := range meta.Sidecar.Secrets {
			declared[s.Name] = s
		}
	}
	values, err := loadSecretsFile(*file)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac secrets import:", err)
		return ExitConfigInvalid
	}
	for k, v := range values {
		spec, ok := declared[k]
		if !ok {
			fmt.Fprintf(env.Stderr, "omac secrets import: %q not declared in sidecar.secrets\n", k)
			return ExitConfigInvalid
		}
		if err := validatePattern(spec, v); err != nil {
			fmt.Fprintln(env.Stderr, "omac secrets import:", err)
			return ExitConfigInvalid
		}
		s := secrets.NewSecretString(v)
		if err := keychain.SetScoped(scope, skill, k, s); err != nil {
			s.Zero()
			fmt.Fprintln(env.Stderr, "omac secrets import: keychain:", err)
			return ExitKeychainError
		}
		s.Zero()
		fmt.Fprintf(env.Stdout, "  stored %s\n", k)
	}
	return ExitOK
}

// secretScopeFor returns the keychain scope for a skill's secrets: "" for a
// global skill (unscoped omac/<skill>), or the workdir-id for a workdir-local
// skill (omac/<workdir-id>/<skill>). This MUST match the scope serve reads
// with (serve.go bringUp). See docs/MULTI_DIR_DESKTOP.md §4.3.
func secretScopeFor(env *Env, global bool) string {
	if global {
		return ""
	}
	return keychain.WorkdirID(env.Workdir)
}

// loadRegisteredMeta resolves a registered skill's meta. It also reports
// whether the skill is registered as user-global (true) or workdir-local
// (false) — the workdir layer wins on a name collision, matching
// mergeRegistries — so callers can pick the right keychain scope.
func loadRegisteredMeta(env *Env, skill string) (*config.Meta, bool, error) {
	workdirReg, err := registry.Load(env.Workdir)
	if err != nil {
		return nil, false, err
	}
	globalReg, err := registry.LoadGlobal()
	if err != nil {
		return nil, false, err
	}
	// Workdir-local wins on collision; it's global only if present in the
	// global registry and NOT the workdir registry.
	wEntry, _ := workdirReg.Find(skill)
	gEntry, _ := globalReg.Find(skill)
	var entry *registry.Entry
	global := false
	if wEntry != nil {
		entry = wEntry
	} else if gEntry != nil {
		entry = gEntry
		global = true
	}
	if entry == nil {
		return nil, false, fmt.Errorf("skill %q is not registered in this workdir or globally", skill)
	}
	absDir := entry.SkillDir
	if !filepath.IsAbs(absDir) {
		absDir = filepath.Join(env.Workdir, absDir)
	}
	m, err := config.LoadMeta(filepath.Join(absDir, config.MetaFileName))
	return m, global, err
}

func findSecret(m *config.Meta, name string) (config.SecretSpec, bool) {
	if m.Sidecar == nil {
		return config.SecretSpec{}, false
	}
	for _, s := range m.Sidecar.Secrets {
		if s.Name == name {
			return s, true
		}
	}
	return config.SecretSpec{}, false
}
