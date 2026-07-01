package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/tngtech/oh-my-agentic-coder/internal/builtinskills"
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/netprompt"
	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxrun"
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

	// Inner harness binary status.
	doctorHarnessBinaries(env)

	// Built-in skills provisioned by `omac setup`, per installed harness.
	doctorBuiltinSkills(env)

	// Sandbox binary.
	lc, _, _ := config.LoadLauncher(env.Workdir)
	profName := lc.Sandbox.DefaultProfile
	if prof, ok := lc.Sandbox.Profiles[profName]; ok && len(prof.Command) > 0 {
		head := prof.Command[0]
		if head == "{{self}}" {
			fmt.Fprintf(env.Stdout, "[ok] sandbox profile %q uses the built-in sandbox\n", profName)
			doctorBuiltinSandbox(env)
		} else if _, err := exec.LookPath(head); err != nil {
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

// doctorBuiltinSkills reports whether omac's built-in skills (provisioned by
// `omac setup`) are present and current in each installed harness's native
// skills dir. It is advisory: a missing/stale/foreign bundle is a warning, not
// a doctor failure.
func doctorBuiltinSkills(env *Env) {
	harnesses := installedHarnesses()
	if len(harnesses) == 0 {
		fmt.Fprintln(env.Stdout, "[warn] built-in skills: no harness detected on $PATH; run `omac setup` after installing one")
		return
	}
	for _, h := range harnesses {
		dir := h.GlobalSkillsDir()
		if dir == "" {
			continue
		}
		for _, name := range builtinskills.Names() {
			st, err := builtinskills.Check(name, dir)
			if err != nil {
				fmt.Fprintf(env.Stdout, "  [warn] built-in %s (%s): %v\n", name, h.Name, err)
				continue
			}
			switch st {
			case builtinskills.StateCurrent:
				fmt.Fprintf(env.Stdout, "  [ok] built-in %s (%s): present\n", name, h.Name)
			case builtinskills.StateMissing:
				fmt.Fprintf(env.Stdout, "  [warn] built-in %s (%s): missing — run `omac setup`\n", name, h.Name)
			case builtinskills.StateStale:
				fmt.Fprintf(env.Stdout, "  [warn] built-in %s (%s): out of date — run `omac setup`\n", name, h.Name)
			case builtinskills.StateForeign:
				fmt.Fprintf(env.Stdout, "  [warn] built-in %s (%s): a non-omac directory occupies that name\n", name, h.Name)
			}
		}
	}
}

// doctorBuiltinSandbox reports the platform prerequisites of the
// built-in sandbox: kernel backend availability (hard requirement) and
// dialog backend availability for the network prompt (warning only).
func doctorBuiltinSandbox(env *Env) {
	if err := sandboxrun.CheckPlatform(); err != nil {
		fmt.Fprintf(env.Stdout, "[fail] built-in sandbox: %v\n", err)
	} else {
		fmt.Fprintln(env.Stdout, "[ok] built-in sandbox: kernel backend available")
	}
	for _, line := range sandboxrun.DoctorNotes() {
		fmt.Fprintln(env.Stdout, line)
	}
	if _, available := netprompt.NewPrompter(1, nil); available {
		fmt.Fprintln(env.Stdout, "[ok] network prompt: dialog backend available")
	} else {
		fmt.Fprintln(env.Stdout, "[warn] network prompt: no dialog backend (osascript/zenity/kdialog); prompts fall back to the on_unavailable policy (default: deny)")
	}
}

// doctorHarnessBinaries reports which harness binaries are on $PATH.
// Advisory only — does not affect the exit code.
func doctorHarnessBinaries(env *Env) {
	fmt.Fprintln(env.Stdout, "Inner harnesses:")
	for _, h := range config.AllHarnesses() {
		if len(h.InnerCmd) == 0 {
			continue
		}
		bin := h.InnerCmd[0]
		if _, err := exec.LookPath(bin); err == nil {
			fmt.Fprintf(env.Stdout, "  [ok]   %-12s binary=%s found\n", h.Name, bin)
		} else {
			fmt.Fprintf(env.Stdout, "  [warn] %-12s binary=%s not on $PATH\n", h.Name, bin)
		}
	}
}
