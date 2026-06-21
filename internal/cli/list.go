package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/tabwriter"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
)

// wellKnownInterpreters is the set of command[0] values that indicate the
// actual skill artifact is command[1]. Used by `omac list` and `omac doctor`
// to produce a useful "binary present" signal when a skill ships a script
// run by a system interpreter.
var wellKnownInterpreters = map[string]bool{
	"python":  true,
	"python3": true,
	"ruby":    true,
	"node":    true,
	"bun":     true,
	"bash":    true,
	"sh":      true,
	"zsh":     true,
	"env":     true, // `env python3 ...` edge case
	"uv":      true, // `uv run script.py`
	"uvx":     true,
}

// skillArtifactCandidate picks the element of command[] that best represents
// "the thing the user built". Falls through to command[0] for native binaries.
func skillArtifactCandidate(command []string) string {
	if len(command) == 0 {
		return ""
	}
	head := filepath.Base(command[0])
	if wellKnownInterpreters[head] && len(command) > 1 {
		// Skip intermediate flags (anything starting with "-"), then take
		// the first non-flag token as the script path.
		for _, t := range command[1:] {
			if len(t) > 0 && t[0] == '-' {
				continue
			}
			return t
		}
	}
	return command[0]
}

func runList(args []string, env *Env) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	showAll := fs.Bool("all", false, "Also list stale registrations whose skill directory no longer exists on disk.")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac list [--all]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}

	workdirReg, err := registry.Load(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac list:", err)
		return ExitIOError
	}
	globalReg, err := registry.LoadGlobal()
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac list:", err)
		return ExitIOError
	}
	// Track which names came from the workdir layer so we can label
	// scope and so the merged view honors "workdir wins".
	workdirNames := map[string]struct{}{}
	for _, e := range workdirReg.Registered {
		workdirNames[e.Name] = struct{}{}
	}
	reg := mergeRegistries(globalReg, workdirReg)
	if len(reg.Registered) == 0 {
		fmt.Fprintln(env.Stdout, "(no skills registered in this workdir or globally)")
		return ExitOK
	}

	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSCOPE\tMOUNT\tSECRETS\tBINARY-PRESENT\tREGISTERED")

	var stale []staleEntry
	shown := 0
	for _, e := range reg.Registered {
		scope := "global"
		if _, ok := workdirNames[e.Name]; ok {
			scope = "workdir"
		}
		// SkillDir is stored relative to the workdir for workdir-local
		// skills and absolute for user-global ones; only join when the
		// stored path isn't already absolute.
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			absDir = filepath.Join(env.Workdir, absDir)
		}
		metaPath := filepath.Join(absDir, config.MetaFileName)
		m, metaErr := config.LoadMeta(metaPath)
		// A skill whose directory / omac.yaml no longer exists is a
		// stale registration: the on-disk skill is gone even though a
		// registry entry (possibly in the global layer) survives. Do
		// NOT list it as a live skill — that was the source of "deleted
		// the .opencode dir but the skill still shows up".
		if metaErr != nil || m.Sidecar == nil {
			stale = append(stale, staleEntry{name: e.Name, scope: scope, dir: absDir})
			if !*showAll {
				continue
			}
		}

		mount := e.Name
		binaryPresent := "?"
		if metaErr == nil && m.Sidecar != nil {
			mount = m.Sidecar.MountOrDefault(e.Name)
			if candidate := skillArtifactCandidate(m.Sidecar.Command); candidate != "" {
				abs := candidate
				if !filepath.IsAbs(abs) {
					abs = filepath.Join(absDir, abs)
				}
				if _, err := os.Stat(abs); err == nil {
					binaryPresent = "yes"
				} else if _, err := exec.LookPath(candidate); err == nil {
					// Falls back to $PATH for tokens like bare "python3".
					binaryPresent = "yes (on $PATH)"
				} else {
					binaryPresent = "no"
				}
			}
		} else {
			mount = "(stale: skill directory missing)"
			binaryPresent = "no"
		}
		fmt.Fprintf(tw, "%s\t%s\t/%s/\t%d\t%s\t%s\n",
			e.Name, scope, mount, len(e.DeclaredSecretNames), binaryPresent,
			e.RegisteredAt.Format("2006-01-02 15:04"))
		shown++
	}
	tw.Flush()

	if shown == 0 && !*showAll {
		fmt.Fprintln(env.Stdout, "(no live skills; only stale registrations remain — see below)")
	}

	// Surface stale registrations and how to clean them up. These are
	// hidden from the live list by default so a deleted skill stops
	// appearing, but we tell the user they exist and how to remove them.
	if len(stale) > 0 {
		fmt.Fprintf(env.Stderr, "\nomac list: %d stale registration(s) whose skill directory no longer exists:\n", len(stale))
		for _, s := range stale {
			delCmd := "omac deregister " + s.name
			if s.scope == "global" {
				delCmd = "omac deregister --global " + s.name
			}
			fmt.Fprintf(env.Stderr, "  %s (%s, was %s) — remove with: %s\n", s.name, s.scope, s.dir, delCmd)
		}
		fmt.Fprintln(env.Stderr, "  or run `omac deregister --prune` to remove all stale registrations at once.")
	}
	return ExitOK
}

// staleEntry is a registry entry whose on-disk skill is gone.
type staleEntry struct {
	name  string
	scope string // "workdir" | "global"
	dir   string
}
