// Package cli provenance command: read-only dump of every effective
// allow/deny entry across network, filesystem, environment, and skills
// subsystems, each row annotated with the config layer it came from.
//
// omac provenance [--profile <ref>] [--json]
//
// Reuses existing loaders (sandboxprofile.Resolve, netprompt.LoadLearnedPolicy,
// registry.Load, PlatformBaseline, EffectiveProtectedPaths) — no new
// resolution logic, just a presentation layer over what the sandbox
// actually enforces.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/tngtech/oh-my-agentic-coder/internal/netprompt"
	"github.com/tngtech/oh-my-agentic-coder/internal/profileaudit"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// provEntry is one row in any provenance section.
type provEntry struct {
	Entry  string `json:"entry"`
	Action string `json:"action"`
	Source string `json:"source"`
}

// profileSource identifies which profile was resolved and where it came from.
type profileSource struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Source string `json:"source"`
}

// networkView holds the effective network policy + entries.
type networkView struct {
	Mode          string      `json:"mode"`
	PromptOn      bool        `json:"prompt_enabled"`
	OnUnavailable string      `json:"on_unavailable"`
	Entries       []provEntry `json:"entries"`
}

// filesystemView holds the effective filesystem policy + entries.
type filesystemView struct {
	WorkdirAccess string      `json:"workdir_access"`
	Entries       []provEntry `json:"entries"`
}

// environmentView holds env-var allow/deny entries.
type environmentView struct {
	Entries []provEntry `json:"entries"`
}

// skillsView holds the registered-skill entries.
type skillsView struct {
	Workdir string      `json:"workdir"`
	Entries []provEntry `json:"entries"`
}

// provenanceView is the top-level payload. JSON mode marshals this
// directly; text mode walks each section.
type provenanceView struct {
	Profile     profileSource   `json:"profile"`
	Network     networkView     `json:"network"`
	Filesystem  filesystemView  `json:"filesystem"`
	Environment environmentView `json:"environment"`
	Skills      skillsView      `json:"skills"`
}

// hardDenyHosts mirrors netproxy.hardDenyHosts (not exported). Kept here
// for provenance display; if the netproxy list changes, update this too.
var provenanceHardDenyHosts = []string{
	"169.254.169.254",
	"metadata.google.internal",
	"metadata.azure.internal",
}

// buildProvenanceView loads the profile, learned decisions, baseline,
// and registry, then assembles a provenanceView. profileRef is a path,
// name, or "" for the default profile.
func buildProvenanceView(workdir, profileRef string) (*provenanceView, error) {
	profile, profPath, err := sandboxprofile.Resolve(profileRef)
	if err != nil {
		return nil, err
	}
	profSource := profileSource{Name: profile.Meta.Name, Path: profPath}
	profSource.Source = classifyProfilePath(profPath, workdir)

	view := &provenanceView{Profile: profSource}

	// --- Network ---
	view.Network = buildNetworkView(profile, profPath, workdir)

	// --- Filesystem ---
	view.Filesystem = buildFilesystemView(profile, profPath, workdir)

	// --- Environment ---
	view.Environment = buildEnvironmentView(profile, profPath, workdir)

	// --- Skills ---
	view.Skills = buildSkillsView(workdir)

	return view, nil
}

// classifyProfilePath attributes a profile path to a config layer.
func classifyProfilePath(profPath, workdir string) string {
	if profPath == "" {
		return "builtin"
	}
	if rel, err := filepath.Rel(filepath.Join(workdir, ".opencode"), profPath); err == nil && !strings.HasPrefix(rel, "..") {
		return "workdir"
	}
	home, err := os.UserHomeDir()
	if err == nil && strings.HasPrefix(profPath, filepath.Join(home, ".config", "omac")) {
		return "global"
	}
	return "global"
}

func buildNetworkView(profile *sandboxprofile.Profile, profPath, workdir string) networkView {
	src := classifyProfilePath(profPath, workdir)
	nv := networkView{
		Mode:          profile.Network.EffectiveMode(),
		PromptOn:      profile.Network.PromptEnabled(),
		OnUnavailable: profile.Network.OnUnavailable(),
	}
	// Learned decisions (from <name>.pages.json).
	pagesPath := sandboxprofile.PagesPath(profPath)
	if lp, err := netprompt.LoadLearnedPolicy(pagesPath); err == nil {
		for _, e := range lp.Entries() {
			nv.Entries = append(nv.Entries, provEntry{
				Entry:  e.Host,
				Action: e.Decision,
				Source: "learned",
			})
		}
	}
	// allow_domain.
	for _, d := range profile.Network.AllowDomain {
		nv.Entries = append(nv.Entries, provEntry{Entry: d, Action: "allow", Source: src})
	}
	// deny_domain.
	for _, d := range profile.Network.DenyDomain {
		nv.Entries = append(nv.Entries, provEntry{Entry: d, Action: "deny", Source: src})
	}
	// Hard-deny metadata hosts (builtin).
	for _, h := range provenanceHardDenyHosts {
		nv.Entries = append(nv.Entries, provEntry{Entry: h, Action: "deny", Source: "builtin"})
	}
	return nv
}

func buildFilesystemView(profile *sandboxprofile.Profile, profPath, workdir string) filesystemView {
	profSrc := classifyProfilePath(profPath, workdir)
	fv := filesystemView{WorkdirAccess: profile.Workdir.Access}
	if fv.WorkdirAccess == "" {
		fv.WorkdirAccess = sandboxprofile.AccessNone
	}
	add := func(entries []string, action, src string) {
		for _, e := range entries {
			fv.Entries = append(fv.Entries, provEntry{Entry: e, Action: action, Source: src})
		}
	}
	add(profile.Filesystem.Allow, "allow", profSrc)
	add(profile.Filesystem.Read, "read", profSrc)
	add(profile.Filesystem.Write, "write", profSrc)
	add(profile.Filesystem.Deny, "deny", profSrc)
	add(profile.Filesystem.OverrideDeny, "override-deny", profSrc)
	for _, d := range profile.Filesystem.AllowUnixDir {
		fv.Entries = append(fv.Entries, provEntry{Entry: d + " (unix-dir)", Action: "allow", Source: profSrc})
	}
	// Baseline read/write.
	baseline := sandboxprofile.PlatformBaseline()
	add(baseline.Read, "read", "builtin")
	add(baseline.Write, "write", "builtin")
	// Effective protected paths.
	for _, p := range sandboxprofile.EffectiveProtectedPaths(baseline, profile.Filesystem.OverrideDeny) {
		fv.Entries = append(fv.Entries, provEntry{Entry: p, Action: "deny", Source: "builtin"})
	}
	return fv
}

func buildEnvironmentView(profile *sandboxprofile.Profile, profPath, workdir string) environmentView {
	ev := environmentView{}
	profSrc := classifyProfilePath(profPath, workdir)
	exact, prefixes := sandboxprofile.DangerousEnvBlocklist()
	for _, name := range exact {
		ev.Entries = append(ev.Entries, provEntry{Entry: name, Action: "deny", Source: "blocklist"})
	}
	for _, p := range prefixes {
		ev.Entries = append(ev.Entries, provEntry{Entry: p + "*", Action: "deny", Source: "blocklist"})
	}
	if len(profile.Environment.AllowVars) == 0 {
		ev.Entries = append(ev.Entries, provEntry{
			Entry:  "(no allowlist — all non-blocklisted vars pass)",
			Action: "allow",
			Source: "default",
		})
	} else {
		for _, v := range profile.Environment.AllowVars {
			ev.Entries = append(ev.Entries, provEntry{Entry: v, Action: "allow", Source: profSrc})
		}
	}
	return ev
}

func buildSkillsView(workdir string) skillsView {
	sv := skillsView{Workdir: workdir}
	workdirReg, err := registry.Load(workdir)
	if err != nil {
		return sv
	}
	globalReg, err := registry.LoadGlobal()
	if err != nil {
		return sv
	}
	workdirNames := map[string]struct{}{}
	for _, e := range workdirReg.Registered {
		workdirNames[e.Name] = struct{}{}
	}
	reg := mergeRegistries(globalReg, workdirReg)
	for _, e := range reg.Registered {
		src := "global"
		if _, ok := workdirNames[e.Name]; ok {
			src = "workdir"
		}
		sv.Entries = append(sv.Entries, provEntry{
			Entry:  e.Name,
			Action: "registered",
			Source: src,
		})
	}
	return sv
}

// writeProvenanceJSON marshals the view as indented JSON.
func writeProvenanceJSON(w io.Writer, v *provenanceView) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "omac provenance: json:", err)
		return ExitIOError
	}
	return ExitOK
}

// writeProvenanceText renders the view as four tabwriter tables.
func writeProvenanceText(w io.Writer, v *provenanceView) int {
	// Network
	fmt.Fprintf(w, "\nnetwork (profile: %s, mode: %s, prompt: %s, on_unavailable: %s)\n",
		v.Profile.Name, v.Network.Mode,
		onOff(v.Network.PromptOn), v.Network.OnUnavailable)
	writeProvTable(w, v.Network.Entries)

	// Filesystem
	fmt.Fprintf(w, "\nfilesystem (profile: %s, workdir.access: %s)\n",
		v.Profile.Name, v.Filesystem.WorkdirAccess)
	writeProvTable(w, v.Filesystem.Entries)

	// Environment
	fmt.Fprintln(w, "\nenvironment")
	writeProvTable(w, v.Environment.Entries)

	// Skills
	fmt.Fprintf(w, "\nskills (workdir: %s)\n", v.Skills.Workdir)
	writeProvTable(w, v.Skills.Entries)
	return ExitOK
}

func writeProvTable(w io.Writer, entries []provEntry) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  ENTRY\tACTION\tSOURCE")
	for _, e := range entries {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", truncateEntry(e.Entry), e.Action, e.Source)
	}
	_ = tw.Flush()
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// truncateEntry truncates display values at 60 chars, appending ….
func truncateEntry(s string) string {
	const max = 60
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// runProvenance implements `omac provenance [--profile <ref>] [--check] [--json]`.
func runProvenance(args []string, env *Env) int {
	fs := flag.NewFlagSet("provenance", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	profileRef := fs.String("profile", "", "sandbox profile name, path, or builtin (default: default)")
	checkMode := fs.Bool("check", false, "Static security lint of the resolved profile.")
	jsonOut := fs.Bool("json", false, "Emit a JSON object instead of tabular text.")
	fs.Usage = func() {
		fmt.Fprintln(env.Stderr, "Usage: omac provenance [--profile <ref>] [--check] [--json]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}

	// --check resolves the profile itself and runs the lint; it does
	// not build the provenance view. Keeps --check independent of the
	// view-build path and its (registry, learned-policy) dependencies.
	if *checkMode {
		profile, _, err := sandboxprofile.Resolve(*profileRef)
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac provenance --check:", err)
			return ExitConfigInvalid
		}
		findings := profileaudit.Check(profile)
		if *jsonOut {
			return writeCheckJSON(env.Stdout, findings)
		}
		return writeCheckText(env.Stdout, findings)
	}

	view, err := buildProvenanceView(env.Workdir, *profileRef)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac provenance:", err)
		return ExitConfigInvalid
	}
	if *jsonOut {
		return writeProvenanceJSON(env.Stdout, view)
	}
	return writeProvenanceText(env.Stdout, view)
}

// writeCheckText renders findings one per line, sorted by severity.
func writeCheckText(w io.Writer, findings []profileaudit.Finding) int {
	if len(findings) == 0 {
		fmt.Fprintln(w, "(no findings)")
		return ExitOK
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  SEVERITY\tFIELD\tVALUE\tMESSAGE")
	for _, f := range findings {
		fmt.Fprintf(tw, "  [%s]\t%s\t%s\t%s\n",
			strings.ToUpper(string(f.Severity)),
			f.Field, f.Value, f.Message)
	}
	_ = tw.Flush()
	return profileaudit.ExitCode(findings)
}

// writeCheckJSON marshals findings as a JSON array.
func writeCheckJSON(w io.Writer, findings []profileaudit.Finding) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(findings); err != nil {
		fmt.Fprintln(os.Stderr, "omac provenance --check: json:", err)
		return ExitIOError
	}
	return profileaudit.ExitCode(findings)
}
