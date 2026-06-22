package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillsource"
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
	harnessName := fs.String("harness", "", "Harness scope for discovering installed skills (opencode|claude). Default: opencode.")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac list [--harness <name>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return ExitMisuse
	}

	harness := config.DefaultHarness()
	if *harnessName != "" {
		h, ok := config.LookupHarness(*harnessName)
		if !ok {
			fmt.Fprintln(env.Stderr, "omac list:", config.UnknownHarnessError(*harnessName))
			return ExitMisuse
		}
		harness = h
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

	// Discover on-disk skills (installed but possibly not registered).
	discovered, err := skillsource.Discover(env.Workdir, harness)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac list: scan skills:", err)
		return ExitIOError
	}
	discoveredByName := map[string]skillsource.Entry{}
	for _, d := range discovered {
		discoveredByName[d.Name] = d
	}

	type listRow struct {
		status        string // "registered" | "installed" | "missing"
		name          string
		scope         string // "workdir" | "global"
		mount         string
		secrets       int
		binaryPresent string
		registered    string // timestamp or "-"
	}

	rowsByName := map[string]*listRow{}
	var order []string

	// Registered skills first.
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
		status := "registered"
		var mount string
		binaryPresent := "?"
		if metaErr != nil || m.Sidecar == nil {
			// On-disk skill is gone even though a registry entry survives.
			status = "missing"
			mount = "(stale: skill directory missing)"
			binaryPresent = "no"
		} else {
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
		}
		rowsByName[e.Name] = &listRow{
			status:        status,
			name:          e.Name,
			scope:         scope,
			mount:         mount,
			secrets:       len(e.DeclaredSecretNames),
			binaryPresent: binaryPresent,
			registered:    e.RegisteredAt.Format("2006-01-02 15:04"),
		}
		order = append(order, e.Name)
	}

	// On-disk skills not in the registry (installed but not registered).
	var installedNames []string
	for _, d := range discovered {
		if _, inReg := rowsByName[d.Name]; !inReg {
			installedNames = append(installedNames, d.Name)
		}
	}
	sort.Strings(installedNames)
	for _, name := range installedNames {
		d := discoveredByName[name]
		mount := name
		secrets := 0
		binaryPresent := "?"
		if m, err := config.LoadMeta(filepath.Join(d.Dir, config.MetaFileName)); err == nil && m.Sidecar != nil {
			mount = m.Sidecar.MountOrDefault(name)
			secrets = len(m.Sidecar.Secrets)
			if candidate := skillArtifactCandidate(m.Sidecar.Command); candidate != "" {
				abs := candidate
				if !filepath.IsAbs(abs) {
					abs = filepath.Join(d.Dir, abs)
				}
				if _, err := os.Stat(abs); err == nil {
					binaryPresent = "yes"
				} else if _, err := exec.LookPath(candidate); err == nil {
					binaryPresent = "yes (on $PATH)"
				} else {
					binaryPresent = "no"
				}
			}
		}
		scope := "workdir"
		if d.Kind == "user-global" {
			scope = "global"
		}
		rowsByName[name] = &listRow{
			status:        "installed",
			name:          name,
			scope:         scope,
			mount:         mount,
			secrets:       secrets,
			binaryPresent: binaryPresent,
			registered:    "-",
		}
		order = append(order, name)
	}

	if len(rowsByName) == 0 {
		fmt.Fprintln(env.Stdout, "(no skills installed or registered in this workdir or globally)")
		return ExitOK
	}

	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tNAME\tSCOPE\tMOUNT\tSECRETS\tBINARY-PRESENT\tREGISTERED")
	for _, name := range order {
		r := rowsByName[name]
		fmt.Fprintf(tw, "%s\t%s\t%s\t/%s/\t%d\t%s\t%s\n",
			r.status, r.name, r.scope, r.mount, r.secrets, r.binaryPresent, r.registered)
	}
	tw.Flush()

	// Surface stale registrations (STATUS=missing) and how to clean them up.
	var stale []string
	for _, name := range order {
		if rowsByName[name].status == "missing" {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		fmt.Fprintf(env.Stderr, "\nomac list: %d stale registration(s) whose skill directory no longer exists:\n", len(stale))
		for _, name := range stale {
			r := rowsByName[name]
			delCmd := "omac deregister " + name
			if r.scope == "global" {
				delCmd = "omac deregister --global " + name
			}
			fmt.Fprintf(env.Stderr, "  %s (%s) — remove with: %s\n", name, r.scope, delCmd)
		}
		fmt.Fprintln(env.Stderr, "  or run `omac deregister --prune` to remove all stale registrations at once.")
	}
	return ExitOK
}
