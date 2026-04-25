package cli

import (
	"flag"
	"fmt"

	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
)

func runDeregister(args []string, env *Env) int {
	fs := flag.NewFlagSet("deregister", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	var (
		purge       = fs.Bool("purge-secrets", false, "Also delete every omac/<skill>/* entry from the keychain.")
		purgeFields = fs.Bool("purge-fields", false, "Also delete this skill's entries from .opencode/skill-config.json.")
	)
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac deregister <skill> [--purge-secrets] [--purge-fields]")
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
	if err := registry.WithLock(env.Workdir, func() error {
		reg, err := registry.Load(env.Workdir)
		if err != nil {
			return err
		}
		if e, _ := reg.Find(name); e != nil {
			declared = e.DeclaredSecretNames
		}
		existed = reg.Remove(name)
		if err := registry.Save(env.Workdir, reg); err != nil {
			return err
		}
		// Field purge sits under the same flock as the registry update
		// so the two .opencode/ files stay consistent.
		if *purgeFields {
			store, err := skillconfig.Load(env.Workdir)
			if err != nil {
				return err
			}
			removedFields = len(store.FieldsFor(name))
			if store.RemoveSkill(name) {
				if err := skillconfig.Save(env.Workdir, store); err != nil {
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
	fmt.Fprintln(env.Stdout)
	return ExitOK
}
