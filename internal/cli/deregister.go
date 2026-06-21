package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
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
		harnessName   = fs.String("harness", "", "Deregister only the entry for this harness (opencode|claude). Default: the first matching entry. Use when a skill name is registered under multiple harnesses.")
		globalOnly    = fs.Bool("global", false, "Force deregistration from the user-global registry (~/.config/omac), not the workdir layer.")
		prune         = fs.Bool("prune", false, "Remove every stale registration (workdir + global) whose skill directory no longer exists. Ignores the <skill> argument.")
	)
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac deregister <skill> [--global] [--harness <name>] [--purge-secrets] [--purge-fields] [--purge-defaults]")
		fmt.Fprintln(env.Stderr, "       omac deregister --prune   # remove all stale registrations")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}
	if *prune {
		if fs.NArg() != 0 {
			fmt.Fprintln(env.Stderr, "omac deregister: --prune takes no <skill> argument")
			return ExitMisuse
		}
		return runDeregisterPrune(env)
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return ExitMisuse
	}
	name := fs.Arg(0)

	// Resolve the optional harness selector. Empty means "first match,
	// any harness" (legacy behavior).
	harnessKey := ""
	if *harnessName != "" {
		h, ok := config.LookupHarness(*harnessName)
		if !ok {
			fmt.Fprintln(env.Stderr, "omac deregister:", config.UnknownHarnessError(*harnessName))
			return ExitMisuse
		}
		harnessKey = h.Name
	}

	var declared []string
	var existed bool
	var removedFields int
	var global bool

	// A skill is registered in exactly one layer: the workdir registry
	// for workdir-local skills, or the user-global registry
	// (~/.config/omac) for user-global skills. With --global the caller
	// forces the global layer; otherwise try the workdir layer first
	// and fall through to the global layer so `omac deregister <name>`
	// works from anywhere for a globally-registered skill.
	if *globalOnly {
		global = true
	} else if gReg, err := registry.LoadGlobal(); err == nil {
		if e, _ := gReg.FindForHarness(name, harnessKey); e != nil {
			if wReg, err := registry.Load(env.Workdir); err == nil {
				if e, _ := wReg.FindForHarness(name, harnessKey); e == nil {
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
		if e, _ := reg.FindForHarness(name, harnessKey); e != nil {
			declared = e.DeclaredSecretNames
		}
		if harnessKey != "" {
			existed = reg.RemoveForHarness(name, harnessKey)
		} else {
			existed = reg.Remove(name)
		}
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

	// Ask a running omac serve to reload so the deregistered skill is dropped
	// without a restart: reload the global layer for a global skill, else the
	// workdir.
	if global {
		if ok, msg := notifyReloadGlobal(); ok {
			fmt.Fprintf(env.Stdout, "[ok] %s\n", msg)
		}
	} else {
		if ok, msg := notifyReload(env.Workdir); ok {
			fmt.Fprintf(env.Stdout, "[ok] %s\n", msg)
		}
	}
	return ExitOK
}

// runDeregisterPrune removes every registry entry, in both the workdir
// and user-global layers, whose skill directory (or its omac.yaml) no
// longer exists on disk. Secrets and config values are kept, matching
// the conservative policy of the start-time auto-prune. Returns the
// names removed.
func runDeregisterPrune(env *Env) int {
	total := 0
	prune := func(global bool) error {
		return withRegistryLock(env.Workdir, global, func() error {
			reg, err := loadRegistry(env.Workdir, global)
			if err != nil {
				return err
			}
			var removed []string
			var keep []registry.Entry
			for _, e := range reg.Registered {
				absDir := e.SkillDir
				if !filepath.IsAbs(absDir) {
					absDir = filepath.Join(env.Workdir, absDir)
				}
				if _, statErr := os.Stat(filepath.Join(absDir, config.MetaFileName)); statErr != nil {
					if errors.Is(statErr, os.ErrNotExist) {
						removed = append(removed, e.Name)
						continue
					}
					return fmt.Errorf("stat %s: %w", e.Name, statErr)
				}
				keep = append(keep, e)
			}
			if len(removed) == 0 {
				return nil
			}
			reg.Registered = keep
			if err := saveRegistry(env.Workdir, global, reg); err != nil {
				return err
			}
			layer := "workdir"
			if global {
				layer = "global"
			}
			for _, name := range removed {
				fmt.Fprintf(env.Stdout, "[ok] pruned stale registration %s (%s)\n", name, layer)
			}
			total += len(removed)
			return nil
		})
	}
	if err := prune(false); err != nil {
		fmt.Fprintln(env.Stderr, "omac deregister --prune:", err)
		return ExitIOError
	}
	if err := prune(true); err != nil {
		fmt.Fprintln(env.Stderr, "omac deregister --prune:", err)
		return ExitIOError
	}
	if total == 0 {
		fmt.Fprintln(env.Stdout, "[noop] no stale registrations found")
		return ExitOK
	}
	// Nudge a running serve to drop the pruned skills.
	if ok, msg := notifyReload(env.Workdir); ok {
		fmt.Fprintf(env.Stdout, "[ok] %s\n", msg)
	}
	if ok, msg := notifyReloadGlobal(); ok {
		fmt.Fprintf(env.Stdout, "[ok] %s\n", msg)
	}
	return ExitOK
}
