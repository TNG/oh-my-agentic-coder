package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
)

func runDoctor(args []string, env *Env) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	_ = fs.Bool("fix", false, "Reserved for future automatic fixes.")
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}

	fmt.Fprintf(env.Stdout, "omac %s\n", env.Version)
	fmt.Fprintf(env.Stdout, "OS: %s\n", osinfo.Detect())
	fmt.Fprintf(env.Stdout, "workdir: %s\n", env.Workdir)

	// Launcher config resolution.
	_, cfgPath, err := config.LoadLauncher(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stdout, "[fail] launcher config:", err)
		return ExitConfigInvalid
	}
	if cfgPath == "" {
		fmt.Fprintln(env.Stdout, "[ok] launcher config: (built-in defaults)")
	} else {
		fmt.Fprintln(env.Stdout, "[ok] launcher config:", cfgPath)
	}

	// Registry. Merge the workdir layer with the user-global layer
	// (workdir wins on name collision), matching what `omac start`
	// resolves.
	workdirReg, err := registry.Load(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stdout, "[fail] registry:", err)
		return ExitIOError
	}
	globalReg, err := registry.LoadGlobal()
	if err != nil {
		fmt.Fprintln(env.Stdout, "[fail] global registry:", err)
		return ExitIOError
	}
	reg := mergeRegistries(globalReg, workdirReg)
	fmt.Fprintf(env.Stdout, "[ok] registry: %d skill(s) registered (%d workdir, %d global)\n",
		len(reg.Registered), len(workdirReg.Registered), len(globalReg.Registered))

	// Per-skill checks.
	failures := 0
	for _, e := range reg.Registered {
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			absDir = filepath.Join(env.Workdir, absDir)
		}
		metaPath := filepath.Join(absDir, config.MetaFileName)
		m, err := config.LoadMeta(metaPath)
		if err != nil {
			fmt.Fprintf(env.Stdout, "  [fail] %s: %v\n", e.Name, err)
			failures++
			continue
		}
		if m.Sidecar == nil {
			fmt.Fprintf(env.Stdout, "  [fail] %s: meta no longer declares a sidecar\n", e.Name)
			failures++
			continue
		}
		// Binary presence (looks for the script/binary the skill actually ships,
		// not e.g. python3 itself).
		binOK := "yes"
		if cand := skillArtifactCandidate(m.Sidecar.Command); cand != "" {
			abs := cand
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(absDir, abs)
			}
			if _, err := os.Stat(abs); err != nil {
				if _, perr := exec.LookPath(cand); perr == nil {
					// On $PATH: acceptable.
				} else {
					binOK = "no"
				}
			}
		} else {
			binOK = "n/a"
		}
		// Secrets status.
		missingReq := 0
		for _, s := range m.Sidecar.Secrets {
			present, err := keychain.Has(e.Name, s.Name)
			if err != nil {
				fmt.Fprintf(env.Stdout, "  [fail] %s: keychain probe: %v\n", e.Name, err)
				failures++
				continue
			}
			if !present && s.IsRequired() {
				missingReq++
			}
		}
		status := "ok"
		if binOK == "no" || missingReq > 0 {
			status = "warn"
		}
		fmt.Fprintf(env.Stdout, "  [%s] %-20s binary=%s missing_required_secrets=%d\n",
			status, e.Name, binOK, missingReq)
	}

	// Sandbox binary.
	lc, _, _ := config.LoadLauncher(env.Workdir)
	profName := lc.Sandbox.DefaultProfile
	if prof, ok := lc.Sandbox.Profiles[profName]; ok && len(prof.Command) > 0 {
		head := prof.Command[0]
		if _, err := exec.LookPath(head); err != nil {
			fmt.Fprintf(env.Stdout, "[warn] sandbox profile %q head %q not on $PATH\n", profName, head)
		} else {
			fmt.Fprintf(env.Stdout, "[ok] sandbox profile %q head %q found\n", profName, head)
		}
	}

	if failures > 0 {
		return ExitConfigInvalid
	}
	return ExitOK
}
