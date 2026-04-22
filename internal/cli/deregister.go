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
	var purge = fs.Bool("purge-secrets", false, "Also delete every omac/<skill>/* entry from the keychain.")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac deregister <skill> [--purge-secrets]")
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
	if err := registry.WithLock(env.Workdir, func() error {
		reg, err := registry.Load(env.Workdir)
		if err != nil {
			return err
		}
		if e, _ := reg.Find(name); e != nil {
			declared = e.DeclaredSecretNames
		}
		existed = reg.Remove(name)
		return registry.Save(env.Workdir, reg)
	}); err != nil {
		fmt.Fprintln(env.Stderr, "omac deregister: registry:", err)
		return ExitIOError
	}

	if *purge {
		if err := keychain.DeleteAll(name, declared); err != nil {
			fmt.Fprintln(env.Stderr, "omac deregister: keychain:", err)
			return ExitKeychainError
		}
		fmt.Fprintf(env.Stdout, "[ok] deregistered %s; deleted %d secret(s) from keychain\n", name, len(declared))
	} else {
		if existed {
			fmt.Fprintf(env.Stdout, "[ok] deregistered %s; kept %d secret(s) in keychain (use --purge-secrets to remove)\n", name, len(declared))
		} else {
			fmt.Fprintf(env.Stdout, "[noop] %s was not registered\n", name)
		}
	}
	return ExitOK
}
