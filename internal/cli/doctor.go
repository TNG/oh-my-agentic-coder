package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/builtinskills"
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/netprompt"
	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
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
	host := osinfo.Detect()
	fmt.Fprintf(env.Stdout, "OS: %s\n", host)
	fmt.Fprintf(env.Stdout, "workdir: %s\n", env.Workdir)

	if err := keychain.Ping(); err != nil {
		if keychain.IsUnavailable(err) {
			fmt.Fprintf(env.Stdout, "[warn] keychain backend: unavailable — %s\n", keychainUnavailableHint(host))
		} else {
			fmt.Fprintln(env.Stdout, "[warn] keychain backend:", err)
		}
	} else {
		fmt.Fprintln(env.Stdout, "[ok] keychain backend: reachable")
	}

	// Launcher config resolution. Retain the first successful result
	// so later sections (sandbox binary, profile warnings) reuse it
	// instead of re-loading.
	firstLauncher, cfgPath, err := config.LoadLauncher(env.Workdir)
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

	// Sandbox binary. Reuse the launcher config resolved above so the
	// first successful LoadLauncher result is authoritative.
	lc := firstLauncher
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

	// Advisory: warn about broad tool-home / cache-root grants in the
	// built-in sandbox profile without mutating it. Warnings never
	// affect the exit code.
	doctorSandboxProfileWarnings(env, lc)

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
	if _, available := netprompt.NewPrompter(1, nil, nil, nil); available {
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

// toolHomeWarning describes a directory whose broad grant (Allow or
// Write, and for cache roots also Read) weakens sandbox isolation.
type toolHomeWarning struct {
	entry       string // the granted path as written in the profile
	access      string // Allow / Read / Write
	impact      string
	remediation string
}

// doctorSandboxProfileWarnings inspects every {{self}} sandbox run
// command in the launcher config, resolves the referenced built-in
// sandbox profile read-only, and warns about broad grants that
// isolate tool caches / cargo credentials / rust toolchains. Warnings
// are advisory: they never increment the failure count and never
// mutate the on-disk profile.
func doctorSandboxProfileWarnings(env *Env, lc config.LauncherConfig) {
	for profName, prof := range lc.Sandbox.Profiles {
		if len(prof.Command) == 0 {
			continue
		}
		ref, ok := inspectBuiltinProfileRef(prof.Command)
		if !ok {
			// Opaque external launcher (nono, no-sandbox-debug, etc.):
			// doctor can't see into its profile, so skip silently.
			continue
		}
		p, _, err := sandboxprofile.ResolveReadOnly(ref)
		if err != nil {
			fmt.Fprintf(env.Stdout, "  [warn] sandbox profile %q: %v\n", profName, err)
			continue
		}
		if len(p.Environment.AllowVars) == 0 {
			fmt.Fprintf(env.Stdout, "  [warn] sandbox profile %q has an empty environment.allow_vars\n", profName)
			fmt.Fprintln(env.Stdout, "         impact:      at launch omac forwards only the operational minimum (HOME, PATH,")
			fmt.Fprintln(env.Stdout, "                      TERM, locale, …); all other ambient env vars — including provider")
			fmt.Fprintln(env.Stdout, "                      tokens and secrets — are NOT passed through, and omac does not")
			fmt.Fprintln(env.Stdout, "                      auto-forward auth vars. This differs from the pre-#102 inherit-")
			fmt.Fprintln(env.Stdout, "                      everything behavior; the harness starts but will not authenticate.")
			fmt.Fprintln(env.Stdout, `         remediation: add the vars the harness needs to allow_vars (see`)
			fmt.Fprintln(env.Stdout, `                      sandboxprofile.DefaultAllowVars), or set allow_vars: ["*"] to forward`)
			fmt.Fprintln(env.Stdout, "                      every ambient var (minus the danger blocklist).")
		}
		warns := profileGrantWarnings(p)
		// Cargo-specific presence warning (mode-000 sentinel files).
		warns = append(warns, cargoSentinelWarnings()...)
		for _, w := range warns {
			fmt.Fprintf(env.Stdout, "  [warn] sandbox profile %q %s %s\n", profName, w.access, w.entry)
			fmt.Fprintf(env.Stdout, "         impact:      %s\n", w.impact)
			fmt.Fprintf(env.Stdout, "         remediation: %s\n", w.remediation)
		}
	}
}

// inspectBuiltinProfileRef looks at a sandbox profile Command argv
// template and, if it is a {{self}} sandbox run invocation, extracts
// the --profile reference. Recognized run forms:
//   - "--profile", "default"   (separate args)
//   - "--profile=default"      (inline)
//   - omitted --profile        (resolves to "default")
//
// Only {{self}} sandbox run commands are inspectable; other sandbox
// subcommands and external launchers are opaque and return ok=false.
func inspectBuiltinProfileRef(command []string) (string, bool) {
	if len(command) < 3 || command[0] != "{{self}}" || command[1] != "sandbox" || command[2] != "run" {
		return "", false
	}
	// Find "--profile" (separate or inline) before "--".
	for i := 3; i < len(command); i++ {
		arg := command[i]
		if arg == "--" {
			break
		}
		if arg == "--profile" && i+1 < len(command) {
			return command[i+1], true
		}
		if strings.HasPrefix(arg, "--profile=") {
			return strings.TrimPrefix(arg, "--profile="), true
		}
	}
	// Omitted --profile resolves to "default".
	return "default", true
}

// profileGrantWarnings returns warnings for broad tool-home and
// cache-root grants in a resolved sandbox profile. Cache roots
// (~/.cache, ~/Library/Caches) are warned for Allow, Read, and Write.
// Tool homes (~/.cargo, ~/.rustup, ~/go) are warned for Allow and
// Write. Cargo is also warned for Read when the grant covers the whole
// Cargo home, which exposes host configuration and credentials.
func profileGrantWarnings(p *sandboxprofile.Profile) []toolHomeWarning {
	var out []toolHomeWarning
	type grant struct {
		access string
		paths  []string
	}
	grantSets := []grant{
		{"Allow", p.Filesystem.Allow},
		{"Read", p.Filesystem.Read},
		{"Write", p.Filesystem.Write},
	}
	for _, g := range grantSets {
		for _, raw := range g.paths {
			expanded, err := sandboxprofile.ExpandPath(raw)
			if err != nil {
				continue
			}
			out = append(out, matchToolHome(raw, expanded, g.access)...)
		}
	}
	return out
}

// matchToolHome checks whether an expanded grant path covers a known
// tool home or cache root, returning a warning per match.
func matchToolHome(rawEntry, expanded string, access string) []toolHomeWarning {
	var out []toolHomeWarning
	home, _ := os.UserHomeDir()

	// Cache roots: warn for Allow, Read, Write.
	cacheRoots := []struct {
		raw, expanded string
	}{
		{"~/.cache", filepath.Join(home, ".cache")},
		{"~/Library/Caches", filepath.Join(home, "Library", "Caches")},
	}
	for _, cr := range cacheRoots {
		if pathsCover(expanded, cr.expanded) {
			out = append(out, toolHomeWarning{
				entry:       cr.raw,
				access:      access,
				impact:      "cache roots are writable/readable inside the sandbox, which can leak host-derived caches and weaken per-project isolation",
				remediation: "remove the broad grant; let the sandbox start empty and grant specific cache subpaths only when needed",
			})
		}
	}

	// Tool homes: warn for Allow and Write. Cargo Read warnings require
	// the grant to cover the whole Cargo home, not only its runtime bin.
	toolHomes := []struct {
		raw, expanded string
		remediation   string
	}{
		{"~/.cargo", filepath.Join(home, ".cargo"), "use an isolated CARGO_HOME and project-local .cargo/config.toml; export CARGO_REGISTRIES_<NAME>_TOKEN in the environment that starts omac. If the sandbox profile sets environment.allow_vars, include that exact variable so the sandboxed harness inherits it; sidecar.env_passthrough configures only the sidecar. NAME is the registry key uppercased with '-' replaced by '_'"},
		{"~/.rustup", filepath.Join(home, ".rustup"), "point RUSTUP_HOME at an isolated location inside the sandbox"},
		{"~/go", filepath.Join(home, "go"), "use GOPATH inside the sandbox or grant only ~/go/bin read"},
	}
	for _, th := range toolHomes {
		if !pathsCover(expanded, th.expanded) {
			continue
		}
		if access == "Allow" || access == "Write" {
			out = append(out, toolHomeWarning{
				entry:       th.raw,
				access:      access,
				impact:      "tool home " + th.raw + " is writable inside the sandbox, exposing host-installed toolchains and credentials",
				remediation: th.remediation,
			})
		}
		if access == "Read" && th.raw == "~/.cargo" && pathsCover(expanded, th.expanded) {
			out = append(out, toolHomeWarning{
				entry:       rawEntry,
				access:      access,
				impact:      "Read access exposes host Cargo configuration and credentials inside the sandbox",
				remediation: th.remediation,
			})
		}
	}

	return out
}

// pathsCover reports whether grantPath covers targetPath (i.e. they
// are equal or targetPath is a proper subpath of grantPath). Uses
// filepath.Rel so comparisons are structural, not raw-string prefixes.
func pathsCover(grantPath, targetPath string) bool {
	if grantPath == targetPath {
		return true
	}
	rel, err := filepath.Rel(grantPath, targetPath)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// If the relative path starts with ".." the target is outside the
	// grant; otherwise it is a subpath and covered.
	return !strings.HasPrefix(rel, "..")
}

// cargoSentinelWarnings checks for the presence of cargo config /
// credential files on the host (by Lstat only — never read) and warns
// that an isolated CARGO_HOME won't pick them up. The warning text
// never includes file contents.
func cargoSentinelWarnings() []toolHomeWarning {
	var out []toolHomeWarning
	home, _ := os.UserHomeDir()
	cargoDir := filepath.Join(home, ".cargo")
	sentinels := []string{
		"config",
		"config.toml",
		"credentials",
		"credentials.toml",
	}
	for _, name := range sentinels {
		p := filepath.Join(cargoDir, name)
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		out = append(out, toolHomeWarning{
			entry:       "~/.cargo/" + name,
			access:      "presence",
			impact:      "host cargo " + name + " exists; an isolated CARGO_HOME inside the sandbox will not use it, so registry credentials and configuration must be supplied explicitly",
			remediation: "add a project-local .cargo/config.toml and export CARGO_REGISTRIES_<NAME>_TOKEN in the environment that starts omac. If the sandbox profile sets environment.allow_vars, include that exact variable so the sandboxed harness inherits it; sidecar.env_passthrough configures only the sidecar. NAME is the registry key uppercased with '-' replaced by '_'; doctor detects presence only and never reads or copies the host file",
		})
	}
	return out
}
