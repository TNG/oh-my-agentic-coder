package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/builtinskills"
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/netprompt"
	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
	"github.com/tngtech/oh-my-agentic-coder/internal/profileaudit"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/registryconf"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxrun"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillconfig"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillstate"
)

func runDoctor(args []string, env *Env) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	_ = fs.Bool("fix", false, "Reserved; not implemented yet (no-op).")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: omac doctor [--fix]")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Run sanity checks: keychain, launcher config, registry, sandbox, dialog backend, harness.")
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args, env); !ok {
		return code
	}

	fmt.Fprintf(env.Stdout, "omac %s\n", env.Version)
	host := osinfo.Detect()
	fmt.Fprintf(env.Stdout, "OS: %s\n", host)
	fmt.Fprintf(env.Stdout, "workdir: %s\n", env.Workdir)

	if err := keychain.Ping(); err != nil {
		if keychain.IsUnavailable(err) {
			fmt.Fprintf(env.Stdout, "[warn] keychain backend: unavailable — %s\n", keychain.UnavailableHint(host))
		} else {
			fmt.Fprintln(env.Stdout, "[warn] keychain backend:", err)
		}
	} else {
		fmt.Fprintln(env.Stdout, "[ok] keychain backend: reachable")
	}

	// Launcher config resolution: report which config file (if any) applies.
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

	// Config stores, merged the same way as the registry, so a stored config
	// value counts as present here exactly as it would at launch. A broken
	// store is reported and treated as empty rather than aborting: doctor's job
	// is to enumerate problems, not to stop at the first one.
	workdirCfg, err := skillconfig.Load(env.Workdir)
	if err != nil {
		fmt.Fprintln(env.Stdout, "[warn] skill-config:", err)
		workdirCfg = &skillconfig.Store{}
	}
	globalCfg, err := skillconfig.LoadGlobal()
	if err != nil {
		fmt.Fprintln(env.Stdout, "[warn] global skill-config:", err)
		globalCfg = &skillconfig.Store{}
	}
	cfgStore := skillstate.MergeConfig(globalCfg, workdirCfg)

	// Per-skill checks.
	//
	// Readiness is resolved through internal/skillstate — the same rule `omac
	// start` applies — so doctor cannot disagree with the launch path about
	// whether a value is present. It used to probe the keychain UNSCOPED while
	// start probed it workdir-scoped with an unscoped fallback, so a secret
	// stored per-workdir made doctor report a missing required secret while
	// start launched happily (issue #174, Failure 3).
	//
	// SkipBundleHash: doctor reports on values, not on drift (that is
	// `omac provenance --check` and start's own gate), so it should not pay a
	// tree walk per skill.
	resolver := skillstate.New(skillstate.Options{
		Scope:          keychain.WorkdirID(env.Workdir),
		SkipBundleHash: true,
	})
	failures := 0
	for _, e := range reg.Registered {
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			absDir = filepath.Join(env.Workdir, absDir)
		}
		armed, problems := resolver.Load(e, absDir, cfgStore)
		// Resolution reads secret plaintext (it is the same code path start
		// uses, which is the point); wipe it as soon as we have counted.
		armed.Zero()

		if p := skillstate.First(problems, skillstate.MetaBroken); p != nil {
			fmt.Fprintf(env.Stdout, "  [fail] %s: %s\n", e.Name, p.Detail)
			failures++
			continue
		}
		// Binary presence (looks for the script/binary the skill actually ships,
		// not e.g. python3 itself).
		binOK := "yes"
		if cand := skillArtifactCandidate(armed.Meta.Sidecar.Command); cand != "" {
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

		// An unreadable secret counts toward missing_required_secrets, but a
		// keychain that cannot ANSWER is reported separately: "set these" is
		// the wrong advice when the remedy is to start a Secret Service.
		//
		// It is deliberately NOT a failure. doctor is a diagnostic that
		// enumerates an environment's state, and a host with no keychain is a
		// supported environment — skills can take their credentials from
		// env_passthrough there. Failing would also make doctor's exit code
		// useless as a health gate on exactly the headless CI runners that most
		// need it (scripts/e2e-readme-onboarding.sh gates its job on it), and
		// the backend's absence is already reported once at the top.
		missingSecrets, missingFields, invalid := 0, 0, 0
		for _, p := range problems {
			switch p.Kind {
			case skillstate.KeychainUnavailable:
				missingSecrets++
			case skillstate.MissingSecret:
				missingSecrets++
			case skillstate.MissingField:
				missingFields++
			case skillstate.InvalidSecret:
				invalid++
			}
		}
		status := "ok"
		if binOK == "no" || missingSecrets > 0 || missingFields > 0 || invalid > 0 {
			status = "warn"
		}
		fmt.Fprintf(env.Stdout, "  [%s] %-20s binary=%s missing_required_secrets=%d missing_required_fields=%d\n",
			status, e.Name, binOK, missingSecrets, missingFields)
		for _, p := range problems {
			switch p.Kind {
			case skillstate.KeychainUnavailable, skillstate.InvalidSecret:
				line := fmt.Sprintf("         %s/%s: %s", p.Skill, p.Field, p.Detail)
				if p.Fix != "" {
					line += " — " + p.Fix
				}
				fmt.Fprintln(env.Stdout, line)
			}
		}
	}

	// Inner harness binary status.
	doctorHarnessBinaries(env)

	// Built-in skills provisioned by `omac setup`, per installed harness.
	doctorBuiltinSkills(env)

	// Inspect the same policy profile a launch would use (sandbox.profile_path,
	// else the built-in "default"), so doctor reflects the real config.
	profileRef := inspectProfileRef(env.Workdir, "")

	// omac always launches its built-in OS sandbox.
	doctorBuiltinSandbox(env, profileRef)

	// Advisory: warn about broad tool-home / cache-root grants in the
	// resolved sandbox profile without mutating it. Warnings never
	// affect the exit code.
	doctorSandboxProfileWarnings(env, profileRef)

	// Static security lint of the resolved policy (advisory). Reuses the
	// same engine as `omac provenance --check` — findings are warnings
	// here, never a doctor failure.
	doctorProfileLint(env, profileRef)

	// Advisory: a private-registry mapping the sandbox cannot see makes
	// scoped installs 404 with no denial anywhere to point at, so nothing
	// else in doctor or diagnose would mention it.
	doctorRegistryConfig(env, profileRef)

	fmt.Fprintln(env.Stdout, "\nWhen a run fails, `omac diagnose` shows what the sandbox blocked and why.")

	if failures > 0 {
		return ExitConfigInvalid
	}
	return ExitOK
}

// doctorProfileLint runs the profileaudit static linter over the resolved
// sandbox profile and prints its findings as advisory warnings. It closes
// the gap where doctor never surfaced the security lint (previously only
// reachable via `omac provenance --check`).
func doctorProfileLint(env *Env, profileRef string) {
	profile, _, err := sandboxprofile.Resolve(profileRef)
	if err != nil {
		return // profile problems are already reported by the sandbox section
	}
	findings := profileaudit.Check(profile)
	if len(findings) == 0 {
		fmt.Fprintln(env.Stdout, "[ok] sandbox profile lint: no findings")
		return
	}
	fmt.Fprintf(env.Stdout, "sandbox profile lint (%d finding(s), advisory):\n", len(findings))
	for _, f := range findings {
		fmt.Fprintf(env.Stdout, "  [%s] %s: %s (%s)\n", f.Severity, f.Field, f.Message, f.Value)
	}
}

// doctorRegistryConfig reports whether ~/.npmrc maps a scope to a private
// registry that the sandbox cannot see. That combination fails in a way no
// other check catches: the masked file yields no denial event, and npm's
// fallback to the public registry returns a plain 404 that reads like "no
// such package" (see #150, #241).
//
// Advisory only — it never affects doctor's exit code.
func doctorRegistryConfig(env *Env, profileRef string) {
	profile, _, err := sandboxprofile.Resolve(profileRef)
	if err != nil {
		return // profile problems are already reported by the sandbox section
	}
	enabled := slices.Contains(profile.Filesystem.RegistryConfig, sandboxprofile.RegistryConfigNPM)
	src, err := registryconf.NPMUserConfig()
	if err != nil {
		return
	}
	overridden := sandboxprofile.BuildOverrideLookup(profile.Filesystem.OverrideDeny)[src]

	notice, err := registryconf.InspectNPM(enabled, overridden)
	if err != nil {
		// The launch path turns the same failure into a projection warning
		// (registryconf.projectNPM), so staying silent here would mean the
		// only place that can warn *before* a run does not.
		fmt.Fprintf(env.Stdout, "[warn] registry config: cannot inspect %s: %v\n", src, err)
		fmt.Fprintf(env.Stdout, "       A private-registry mapping in that file cannot be projected, so scoped\n")
		fmt.Fprintf(env.Stdout, "       installs may fail with a 404 against the public registry.\n")
		return
	}
	if notice == nil {
		return
	}
	hosts := strings.Join(notice.Hosts, ", ")
	// Each condition is reported on its own: a profile can have BOTH
	// registry_config and override_deny, and reporting only the former
	// ("[ok] … projected") would reassure the user while the real
	// token-bearing file stays readable by the sandbox.
	switch {
	case len(notice.Hosts) == 0:
		// Only rejections to report; the mapping list is empty.
	case notice.Enabled:
		fmt.Fprintf(env.Stdout, "[ok] registry config: %s mappings (%s) are projected into the sandbox\n",
			notice.Ecosystem, hosts)
	default:
		fmt.Fprintf(env.Stdout, "[warn] registry config: %s maps a scope to %s, but the sandbox cannot read it\n",
			notice.Source, hosts)
		fmt.Fprintf(env.Stdout, "       Scoped installs will fail with a 404 against the public registry. Fix:\n")
		fmt.Fprintf(env.Stdout, "       add filesystem.registry_config: [%q] to the sandbox profile%s.\n",
			notice.Ecosystem, credentialNote(notice.Credentialed))
	}

	if notice.Overridden {
		fmt.Fprintf(env.Stdout, "[warn] registry config: %s is exposed to the sandbox via filesystem.override_deny\n",
			notice.Source)
		fmt.Fprintf(env.Stdout, "       That grants the whole file%s.\n", credentialSuffix(notice.Credentialed))
		if notice.Enabled {
			fmt.Fprintf(env.Stdout, "       filesystem.registry_config is already projecting the mappings, so this grant\n")
			fmt.Fprintf(env.Stdout, "       is redundant — drop it to keep the credential protected.\n")
		} else {
			fmt.Fprintf(env.Stdout, "       Prefer filesystem.registry_config: [%q], which projects only the registry\n", notice.Ecosystem)
			fmt.Fprintf(env.Stdout, "       mappings and drops every credential.\n")
		}
	}

	// Rejections are the silent-failure case: config exists, omac will not
	// use it, and nothing else would say so.
	for _, r := range notice.Rejected {
		fmt.Fprintf(env.Stdout, "[warn] registry config: %s cannot be projected from %s\n", r.Key, notice.Source)
		fmt.Fprintf(env.Stdout, "       %s\n", r.Reason)
	}
}

// credentialSuffix describes what an override_deny grant exposes.
func credentialSuffix(credentialed bool) string {
	if credentialed {
		return ", including the auth token it holds"
	}
	return ""
}

// credentialNote explains why the projection beats the blunt alternative.
func credentialNote(credentialed bool) string {
	if credentialed {
		return " (the file also holds an auth token, so override_deny would expose it)"
	}
	return ""
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
func doctorBuiltinSandbox(env *Env, profileRef string) {
	if err := sandboxrun.CheckPlatform(); err != nil {
		fmt.Fprintf(env.Stdout, "[fail] built-in sandbox: %v\n", err)
	} else {
		fmt.Fprintln(env.Stdout, "[ok] built-in sandbox: kernel backend available")
	}
	for _, line := range sandboxrun.DoctorNotes(profileRef) {
		fmt.Fprintln(env.Stdout, line)
	}
	if _, available := netprompt.NewPrompter(1, nil, nil, nil, nil, nil); available {
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

// doctorSandboxProfileWarnings resolves the sandbox policy profile read-only
// and warns about broad grants that fail to isolate tool caches / cargo
// credentials / rust toolchains, or an env allow/deny list that would break the
// harness. Warnings are advisory: they never increment the failure count and
// never mutate the on-disk profile.
func doctorSandboxProfileWarnings(env *Env, profileRef string) {
	p, path, err := sandboxprofile.Resolve(profileRef)
	if err != nil {
		fmt.Fprintf(env.Stdout, "  [warn] sandbox profile: %v\n", err)
		return
	}
	if len(p.Environment.AllowVars) == 0 {
		fmt.Fprintf(env.Stdout, "  [warn] sandbox profile %q has an empty environment.allow_vars\n", "default")
		if path != "" {
			fmt.Fprintf(env.Stdout, "         source:      %s\n", path)
		}
		fmt.Fprintln(env.Stdout, "         impact:      at launch omac forwards only the operational minimum (HOME, PATH,")
		fmt.Fprintln(env.Stdout, "                      TERM, locale, …); all other ambient env vars — including provider")
		fmt.Fprintln(env.Stdout, "                      tokens and secrets — are NOT passed through, and omac does not")
		fmt.Fprintln(env.Stdout, "                      auto-forward auth vars. This differs from the pre-#102 inherit-")
		fmt.Fprintln(env.Stdout, "                      everything behavior; the harness starts but will not authenticate.")
		fmt.Fprintln(env.Stdout, "         remediation: custom profiles are not updated by omac upgrades; refresh this profile")
		fmt.Fprintln(env.Stdout, "                      from its installer or original source. If you maintain it manually,")
		fmt.Fprintln(env.Stdout, "                      add the vars the harness needs to allow_vars (see")
		fmt.Fprintln(env.Stdout, "                      sandboxprofile.DefaultAllowVars).")
		fmt.Fprintln(env.Stdout, `                      allow_vars: ["*"] is not recommended because it forwards almost`)
		fmt.Fprintln(env.Stdout, "                      every ambient var (minus the danger blocklist).")
	}
	if denied := sandboxprofile.DeniedBaseVars(p.Environment.DenyVars); len(denied) > 0 {
		fmt.Fprintf(env.Stdout, "  [warn] sandbox profile %q denies operational base var(s): %s\n", "default", strings.Join(denied, ", "))
		fmt.Fprintln(env.Stdout, "         impact:      deny_vars wins over everything (allowlist, \"*\", and omac's injected")
		fmt.Fprintln(env.Stdout, "                      overlay), so these are stripped. They are the operational minimum a")
		fmt.Fprintln(env.Stdout, "                      sandboxed harness needs (HOME/PATH/TERM/…); removing them will likely")
		fmt.Fprintln(env.Stdout, "                      break it.")
		fmt.Fprintln(env.Stdout, "         remediation: drop these entries from environment.deny_vars unless the removal is")
		fmt.Fprintln(env.Stdout, "                      deliberate.")
	}
	warns := profileGrantWarnings(p)
	// Cargo-specific presence warning (mode-000 sentinel files).
	warns = append(warns, cargoSentinelWarnings()...)
	for _, w := range warns {
		fmt.Fprintf(env.Stdout, "  [warn] sandbox profile %q %s %s\n", "default", w.access, w.entry)
		fmt.Fprintf(env.Stdout, "         impact:      %s\n", w.impact)
		fmt.Fprintf(env.Stdout, "         remediation: %s\n", w.remediation)
	}
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
