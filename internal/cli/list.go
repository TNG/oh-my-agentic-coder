package cli

import (
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

func runList(_ []string, env *Env) int {
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
	for _, e := range reg.Registered {
		scope := "global"
		if _, ok := workdirNames[e.Name]; ok {
			scope = "workdir"
		}
		mount := e.Name
		binaryPresent := "?"
		// SkillDir is stored relative to the workdir for workdir-local
		// skills and absolute for user-global ones; only join when the
		// stored path isn't already absolute.
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			absDir = filepath.Join(env.Workdir, absDir)
		}
		metaPath := filepath.Join(absDir, config.MetaFileName)
		if m, err := config.LoadMeta(metaPath); err == nil && m.Sidecar != nil {
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
		fmt.Fprintf(tw, "%s\t%s\t/%s/\t%d\t%s\t%s\n",
			e.Name, scope, mount, len(e.DeclaredSecretNames), binaryPresent,
			e.RegisteredAt.Format("2006-01-02 15:04"))
	}
	tw.Flush()
	return ExitOK
}
