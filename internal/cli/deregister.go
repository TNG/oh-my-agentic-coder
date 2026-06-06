package cli

import (
	"flag"
	"fmt"

	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
)

func runDeregister(args []string, env *Env) int {
	fs := flag.NewFlagSet("deregister", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var (
		purge         = fs.Bool("purge-secrets", false, "Also delete every omac/<skill>/* entry from the keychain.")
		purgeFields   = fs.Bool("purge-fields", false, "Also delete this skill's entries from .opencode/skill-config.yaml.")
		purgeDefaults = fs.Bool("purge-defaults", false, "Also delete this skill's remembered global defaults (secrets + config).")
	)
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac deregister <skill> [--purge-secrets] [--purge-fields] [--purge-defaults]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return ExitMisuse
	}
	name := fs.Arg(0)

	var declared []string
	var existed bool
	var removedFields int
	var global bool

	// A skill is registered in exactly one layer: the workdir registry
	// for workdir-local skills, or the user-global registry
	// (~/.config/omac) for user-global skills. Try the workdir layer
	// first; if the skill isn't there, fall through to the global
	// layer so `omac deregister <name>` works from anywhere for a
	// globally-registered skill.
	if gReg, err := registry.LoadGlobal(); err == nil {
		if e, _ := gReg.Find(name); e != nil {
			if wReg, err := registry.Load(env.Workdir); err == nil {
				if e, _ := wReg.Find(name); e == nil {
					global = true
				}
			}
		}
	}

	if err := withRegistryLock(env.Workdir, global, func() error {
		reg, err := loadRegistry(env.Workdir, global)
		if err != nil {
			return err
		}
		if e, _ := reg.Find(name); e != nil {
			declared = e.DeclaredSecretNames
		}
		existed = reg.Remove(name)
		if err := saveRegistry(env.Workdir, global, reg); err != nil {
			return err
		}
		// Field purge sits under the same flock as the registry update
		// so the two files stay consistent.
		if *purgeFields {
			store, err := loadSkillConfig(env.Workdir, global)
			if err != nil {
				return err
			}
			removedFields = len(store.FieldsFor(name))
			if store.RemoveSkill(name) {
				if err := saveSkillConfig(env.Workdir, global, store); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintln(env.Stderr, "omac deregister:", err)
		return ExitIOError
	}

	if *purge {
		if err := keychain.DeleteAll(name, declared); err != nil {
			fmt.Fprintln(env.Stderr, "omac deregister: keychain:", err)
			return ExitKeychainError
		}
		fmt.Fprintf(env.Stdout, "[ok] deregistered %s; deleted %d secret(s) from keychain", name, len(declared))
	} else if existed {
		fmt.Fprintf(env.Stdout, "[ok] deregistered %s; kept %d secret(s) in keychain (use --purge-secrets to remove)", name, len(declared))
	} else {
		fmt.Fprintf(env.Stdout, "[noop] %s was not registered", name)
	}
	if *purgeFields {
		fmt.Fprintf(env.Stdout, "; deleted %d config field(s)", removedFields)
	} else if existed {
		fmt.Fprintf(env.Stdout, " (use --purge-fields to also drop config fields)")
	}

	// Purge remembered global defaults (docs/MULTI_DIR_DESKTOP.md §4.4):
	// the secret defaults under omac/__defaults__/<skill> and the config
	// defaults block in the global skill-config.yaml.
	if *purgeDefaults {
		_ = keychain.DeleteAllScoped(keychain.DefaultsScope, name, declared)
		if err := registry.WithGlobalLock(func() error {
			store, err := loadSkillConfig(env.Workdir, true)
			if err != nil {
				return err
			}
			if store.RemoveDefaults(name) {
				return saveSkillConfig(env.Workdir, true, store)
			}
			return nil
		}); err != nil {
			fmt.Fprintln(env.Stderr, "\nomac deregister: purge defaults:", err)
			return ExitIOError
		}
		fmt.Fprintf(env.Stdout, "; purged remembered defaults")
	}
	fmt.Fprintln(env.Stdout)

	// Ask a running omac serve to reload this directory so the deregistered
	// skill is dropped without a restart (workdir-local only; global skills
	// only re-activate at serve cold start).
	if !global {
		if ok, msg := notifyReload(env.Workdir); ok {
			fmt.Fprintf(env.Stdout, "[ok] %s\n", msg)
		}
	}
	return ExitOK
}
