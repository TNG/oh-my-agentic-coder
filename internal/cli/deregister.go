package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/keychain"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/skillsource"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
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
		assumeYes     = fs.Bool("yes", false, "Do not prompt before deleting an unregistered skill's source directory.")
	)
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, "Usage: omac deregister <skill> [--global] [--harness <name>] [--yes] [--purge-secrets] [--purge-fields] [--purge-defaults]")
		fmt.Fprintln(out, "       omac deregister --prune   # remove all stale registrations")
		fmt.Fprintln(out, "\nRemoves the skill from the registry. If the skill was never registered but")
		fmt.Fprintln(out, "still exists on disk (so `omac start` keeps flagging it), its source directory")
		fmt.Fprintln(out, "is deleted instead (after confirmation, or immediately with --yes).")
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args, env); !ok {
		return code
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
	var revokeHash string
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
		// Capture the entry in the same branch that removes it, so revokeHash
		// always belongs to the entry actually deleted: a name-only Remove
		// deletes any-harness, so it must pair with Find, not the legacy-only
		// FindForHarness. Fields are read BEFORE removal, which reslices
		// reg.Registered and would invalidate the pointer.
		var removing *registry.Entry
		if harnessKey != "" {
			removing, _ = reg.FindForHarness(name, harnessKey)
			if removing != nil {
				declared, revokeHash = removing.DeclaredSecretNames, removing.BundleHash
			}
			existed = reg.RemoveForHarness(name, harnessKey)
		} else {
			removing, _ = reg.Find(name)
			if removing != nil {
				declared, revokeHash = removing.DeclaredSecretNames, removing.BundleHash
			}
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

	// Revoke the host-only spawn approval (see internal/skilltrust) for this
	// entry's exact hash, so a copy still registered under another harness or
	// workdir keeps its own approval. Best-effort: a stale approval is inert
	// on its own (nothing spawns a skill absent from the registry).
	if existed && revokeHash != "" {
		if _, rerr := skilltrust.Revoke(name, revokeHash); rerr != nil {
			fmt.Fprintf(env.Stderr, "omac deregister: could not revoke host approval (%v)\n", rerr)
		}
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
		// Not in the registry. The skill may still exist on disk as a
		// discovered-but-unregistered skill (omac start refuses to run
		// with those). `omac deregister <skill>` should still get rid of
		// it: locate its source directory and remove it. This is
		// destructive, so confirm unless --yes / a non-interactive
		// stdin says otherwise.
		if removed, dir, derr := deleteUnregisteredSource(env, name, harnessKey, *assumeYes); derr != nil {
			fmt.Fprintln(env.Stderr, "\nomac deregister:", derr)
			return ExitIOError
		} else if removed {
			fmt.Fprintf(env.Stdout, "[ok] deleted unregistered skill %s (removed %s)", name, dir)
		} else if dir != "" {
			// Found on disk but the user declined.
			fmt.Fprintf(env.Stdout, "[noop] %s left in place at %s", name, dir)
		} else {
			fmt.Fprintf(env.Stdout, "[noop] %s was not registered and no skill of that name was found on disk", name)
		}
	}
	if *purgeFields {
		fmt.Fprintf(env.Stdout, "; deleted %d config field(s)", removedFields)
	} else if existed {
		fmt.Fprintf(env.Stdout, " (use --purge-fields to also drop config fields)")
	}

	// Purge remembered global defaults (docs/contributing/serve-spec.md):
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

// deleteUnregisteredSource handles `omac deregister <skill>` when the
// skill has no registry entry but still exists on disk as a discovered
// skill (which is exactly what makes `omac start` refuse to run). It
// locates the skill's source directory and deletes it.
//
// Returns (removed, dir, err): removed is true when the directory was
// deleted; dir is the source directory found (empty when no skill of
// that name exists on disk); a non-nil err is an I/O failure.
//
// harnessKey scopes discovery when the caller passed --harness; empty
// means search every harness's scope so `omac deregister <skill>`
// works without the user having to remember which harness owns it.
func deleteUnregisteredSource(env *Env, name, harnessKey string, assumeYes bool) (bool, string, error) {
	harnesses := config.AllHarnesses()
	if harnessKey != "" {
		if h, ok := config.LookupHarness(harnessKey); ok {
			harnesses = []config.Harness{h}
		}
	}
	dir := ""
	for _, h := range harnesses {
		if d, _, err := skillsource.Resolve(env.Workdir, h, name); err == nil {
			dir = d
			break
		}
	}
	if dir == "" {
		return false, "", nil
	}
	if !assumeYes {
		fmt.Fprintf(env.Stdout, "%s is not registered but exists on disk at:\n  %s\nDelete this directory? [y/N] ", name, dir)
		reader := bufio.NewReader(env.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			return false, dir, nil
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, dir, fmt.Errorf("delete %s: %w", dir, err)
	}
	return true, dir, nil
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
