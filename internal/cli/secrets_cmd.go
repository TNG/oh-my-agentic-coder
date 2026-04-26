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
	meta, err := loadRegisteredMeta(env, skill)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac secrets list:", err)
		return ExitPrerequisiteMissing
	}
	if meta.Sidecar == nil || len(meta.Sidecar.Secrets) == 0 {
		fmt.Fprintln(env.Stdout, "(no secrets declared)")
		return ExitOK
	}
	for _, s := range meta.Sidecar.Secrets {
		present, err := keychain.Has(skill, s.Name)
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
	meta, err := loadRegisteredMeta(env, skill)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac secrets set:", err)
		return ExitPrerequisiteMissing
	}
	spec, ok := findSecret(meta, name)
	if !ok {
		fmt.Fprintf(env.Stderr, "omac secrets set: %q does not declare a secret named %q\n", skill, name)
		return ExitConfigInvalid
	}
	// Always re-prompt.
	if err := handleOneSecret(env, skill, spec, true, nil); err != nil {
		fmt.Fprintln(env.Stderr, "omac secrets set:", err)
		return ExitKeychainError
	}
	return ExitOK
}

func runSecretsUnset(args []string, env *Env) int {
	if len(args) != 2 {
		fmt.Fprintln(env.Stderr, "Usage: omac secrets unset <skill> <name>")
		return ExitMisuse
	}
	skill, name := args[0], args[1]
	if err := keychain.Delete(skill, name); err != nil {
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
	meta, err := loadRegisteredMeta(env, skill)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac secrets import:", err)
		return ExitPrerequisiteMissing
	}
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
		if err := keychain.Set(skill, k, s); err != nil {
			s.Zero()
			fmt.Fprintln(env.Stderr, "omac secrets import: keychain:", err)
			return ExitKeychainError
		}
		s.Zero()
		fmt.Fprintf(env.Stdout, "  stored %s\n", k)
	}
	return ExitOK
}

// loadRegisteredMeta looks up the skill in the workdir's registry and loads its meta.
func loadRegisteredMeta(env *Env, skill string) (*config.Meta, error) {
	reg, err := registry.Load(env.Workdir)
	if err != nil {
		return nil, err
	}
	entry, _ := reg.Find(skill)
	if entry == nil {
		return nil, fmt.Errorf("skill %q is not registered in this workdir", skill)
	}
	// SkillDir is stored relative to the workdir for workdir-local
	// skills and absolute for user-global ones; only join when the
	// stored path isn't already absolute.
	absDir := entry.SkillDir
	if !filepath.IsAbs(absDir) {
		absDir = filepath.Join(env.Workdir, absDir)
	}
	return config.LoadMeta(filepath.Join(absDir, "meta.yaml"))
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
