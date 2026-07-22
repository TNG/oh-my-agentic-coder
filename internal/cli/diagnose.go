// Package cli diagnose command: the reactive counterpart to `omac doctor`.
// Where doctor answers "is my setup correct?", diagnose answers "why did my
// run fail?" — it reads the (otherwise write-only) audit trail back and
// correlates the network decisions the sandbox actually made against the
// effective network policy, surfacing blocked hosts and non-obvious config
// clashes.
//
// This file is the composition root: it adapts the concrete, change-prone
// omac types (audit.Event, sandboxprofile.Profile, netproxy's matcher) into
// the standalone internal/diagnose DTOs and injects the domain matcher, so
// the analysis package stays free of omac dependencies.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/diagnose"
	"github.com/tngtech/oh-my-agentic-coder/internal/netproxy"
	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxrun"
)

func runDiagnose(args []string, env *Env) int {
	fs := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	asJSON := fs.Bool("json", false, "Emit machine-readable JSON.")
	profileRef := fs.String("profile", "", "Sandbox profile ref (path or name); default resolves like `omac start`.")
	runSel := fs.String("run", "last", "Which run(s) to analyze: last|all.")
	probe := fs.String("probe", "", "Statically check whether host[:port] would be admitted, then exit.")
	verbose := fs.Bool("verbose", false, "Show every hint and the full effective config, not just the focused view.")
	fs.BoolVar(verbose, "v", false, "Alias for --verbose.")
	if err := fs.Parse(reorderFlagsFirst(args)); err != nil {
		return ExitMisuse
	}
	if *runSel != "last" && *runSel != "all" {
		fmt.Fprintln(env.Stderr, "omac diagnose: --run must be last|all")
		return ExitMisuse
	}

	profile, profPath, err := sandboxprofile.Resolve(*profileRef)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac diagnose:", err)
		return ExitConfigInvalid
	}

	if *probe != "" {
		return runDiagnoseProbe(env, profile, profPath, *probe, *asJSON)
	}

	pol := policyFromProfile(profile)

	// Resolve the audit log path exactly as the writer does (single
	// source of truth: audit.EffectivePath), then read it back.
	lc, _, _ := config.LoadLauncher(env.Workdir)
	auditPath := audit.EffectivePath(audit.Config{
		Enabled: lc.Audit.AuditEnabled(),
		Path:    lc.Audit.Path,
	})

	var (
		events    []audit.Event
		readErr   error
		auditNote string
	)
	if auditPath == "" {
		auditNote = "audit trail is disabled — enable it (config: audit.enabled) to diagnose network decisions"
	} else if events, readErr = audit.ReadFile(auditPath); readErr != nil {
		if os.IsNotExist(readErr) {
			auditNote = "no audit trail yet — run `omac start` first, then re-run `omac diagnose`"
		} else {
			fmt.Fprintln(env.Stderr, "omac diagnose: read audit log:", readErr)
			return ExitIOError
		}
	}

	runID := ""
	if *runSel == "last" {
		runID = audit.LastRunID(events)
	}
	decisions := decisionsFromEvents(events, runID)
	report := diagnose.Build(pol, decisions, netproxy.MatchDomainList)

	logPath, _ := sandboxrun.DiagLogPath()
	view := diagnoseView{
		Version:     env.Version,
		OS:          osinfo.Detect().String(),
		Workdir:     env.Workdir,
		ProfileName: profile.Meta.Name,
		ProfilePath: profPath,
		Policy:      pol,
		RunScope:    *runSel,
		RunID:       runID,
		ExitCode:    runExitCode(events, runID),
		AuditLog:    auditPath,
		SandboxLog:  logPath,
		AuditNote:   auditNote,
		Report:      report,
	}

	if *asJSON {
		return writeDiagnoseJSON(env, view)
	}
	writeDiagnoseText(env, view, *verbose)
	return ExitOK
}

// runExitCode returns the exit code recorded by the session.stop event for
// the scoped run, or nil when unknown. It lets diagnose lead with "the last
// run exited N", framing the evidence around the failure the user is chasing.
func runExitCode(events []audit.Event, runID string) *int {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Type != audit.TypeSessionStop {
			continue
		}
		if runID != "" && ev.RunID != runID {
			continue
		}
		return ev.ExitCode
	}
	return nil
}

// diagnoseView is the full result payload (text + JSON share it). Profile
// state is carried as the diagnose.Policy plus its name/path — no separate
// display struct.
type diagnoseView struct {
	Version     string          `json:"version"`
	OS          string          `json:"os"`
	Workdir     string          `json:"workdir"`
	ProfileName string          `json:"profile_name"`
	ProfilePath string          `json:"profile_path"`
	Policy      diagnose.Policy `json:"policy"`
	RunScope    string          `json:"run_scope"`
	RunID       string          `json:"run_id,omitempty"`
	ExitCode    *int            `json:"exit_code,omitempty"`
	AuditLog    string          `json:"audit_log,omitempty"`
	SandboxLog  string          `json:"sandbox_log,omitempty"`
	AuditNote   string          `json:"audit_note,omitempty"`
	Report      diagnose.Report `json:"report"`
}

// policyFromProfile adapts the sandbox profile into the diagnose Policy
// DTO. This is the single profile->analysis mapping and, with the audit
// adapter below, the only code that knows the concrete omac schemas.
func policyFromProfile(p *sandboxprofile.Profile) diagnose.Policy {
	return diagnose.Policy{
		Mode:          p.Network.EffectiveMode(),
		AllowDomains:  p.Network.AllowDomain,
		DenyDomains:   p.Network.DenyDomain,
		PromptEnabled: p.Network.PromptEnabled(),
		UpstreamProxy: p.Network.UpstreamProxy,
		AllowVars:     p.Environment.AllowVars,
	}
}

// decisionsFromEvents maps net.decision audit events to diagnose Decisions,
// scoped to a single run when runID is set.
func decisionsFromEvents(events []audit.Event, runID string) []diagnose.Decision {
	var out []diagnose.Decision
	for _, ev := range events {
		if ev.Type != audit.TypeNetDecision {
			continue
		}
		if runID != "" && ev.RunID != runID {
			continue
		}
		out = append(out, diagnose.Decision{
			Host:    ev.Host,
			Port:    ev.Port,
			Allowed: ev.Allow != nil && *ev.Allow,
			Source:  ev.Source,
		})
	}
	return out
}

func writeDiagnoseJSON(env *Env, v any) int {
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(env.Stderr, "omac diagnose:", err)
		return ExitIOError
	}
	return ExitOK
}

// maxBlockedShown caps the blocked-host list in the focused view so a noisy
// run does not scroll the actionable findings off-screen. -v shows all.
const maxBlockedShown = 8

// writeDiagnoseText renders the focused view by default: a one-line status,
// the blocked hosts, the problems to act on, and a single collapsed line for
// least-privilege/context advisories. -v expands everything (all advisories,
// full effective config, all log paths). The goal is that a user sees what
// they came for without a wall of warn/info noise.
func writeDiagnoseText(env *Env, v diagnoseView, verbose bool) {
	w := env.Stdout
	fmt.Fprintf(w, "omac %s · %s · %s\n", v.Version, v.OS, v.Workdir)

	// One status line: run/exit + mode + blocked count.
	scope := "last run"
	if v.RunScope == "all" {
		scope = "all runs"
	}
	status := scope
	// The exit code identifies a single run; under --run all it would be the
	// latest run's code mislabeled as "all runs", so only show it for a
	// single-run scope.
	if v.ExitCode != nil && v.RunScope != "all" {
		status += fmt.Sprintf(" exited %d", *v.ExitCode)
	}
	status += fmt.Sprintf(" · %s mode · %d/%d connection(s) blocked", v.Policy.Mode, v.Report.Denied, v.Report.Total)
	fmt.Fprintln(w, status)
	fmt.Fprintln(w)

	if v.AuditNote != "" {
		fmt.Fprintf(w, "note: %s\n\n", v.AuditNote)
	}

	// Blocked hosts (capped unless -v).
	if len(v.Report.Blocked) > 0 {
		fmt.Fprintln(w, "Blocked connections:")
		shown := v.Report.Blocked
		if !verbose && len(shown) > maxBlockedShown {
			shown = shown[:maxBlockedShown]
		}
		for _, b := range shown {
			fmt.Fprintf(w, "  %d×  %-40s (%s)\n", b.Count, hostPorts(b.Host, b.Ports), reasonWord(b.Sources))
		}
		if n := len(v.Report.Blocked) - len(shown); n > 0 {
			fmt.Fprintf(w, "  … and %d more (run with -v)\n", n)
		}
		fmt.Fprintln(w)
	}

	// Problems: the failure-explaining hints, shown in full — this is what
	// the user came for.
	problems := v.Report.Problems()
	if len(problems) > 0 {
		fmt.Fprintln(w, "What to look at:")
		for _, h := range problems {
			fmt.Fprintf(w, "  • %s\n", h.Title)
			for _, d := range h.Detail {
				fmt.Fprintf(w, "    %s\n", d)
			}
		}
		fmt.Fprintln(w)
	} else if len(v.Report.Blocked) == 0 {
		fmt.Fprintln(w, "No problems detected.")
		fmt.Fprintln(w)
	}

	// Advisories: collapsed by default, expanded under -v.
	advisories := v.Report.Advisories()
	if len(advisories) > 0 {
		if verbose {
			fmt.Fprintln(w, "Advisories (least-privilege / context):")
			for _, h := range advisories {
				fmt.Fprintf(w, "  • %s\n", h.Title)
				for _, d := range h.Detail {
					fmt.Fprintf(w, "    %s\n", d)
				}
			}
		} else {
			fmt.Fprintf(w, "%d advisory note(s) about config hygiene / least privilege — run `omac diagnose -v` to see them.\n", len(advisories))
		}
		fmt.Fprintln(w)
	}

	if verbose {
		writeEffectiveConfig(w, v.ProfileName, v.ProfilePath, v.Policy)
	}

	// Footer: where to dig further.
	if verbose && v.AuditLog != "" {
		fmt.Fprintf(w, "audit trail: %s\n", v.AuditLog)
	}
	if verbose && v.SandboxLog != "" {
		fmt.Fprintf(w, "raw sandbox log (TTY sessions): %s\n", v.SandboxLog)
	}
	fmt.Fprintln(w, "More: `omac diagnose -v` (detail) · `omac diagnose --probe <host[:port]>` (test a host) · `omac provenance` (full config)")
}

// writeEffectiveConfig prints the resolved network policy (verbose only). The
// authoritative, all-subsystem view remains `omac provenance`.
func writeEffectiveConfig(w io.Writer, name, path string, p diagnose.Policy) {
	fmt.Fprintf(w, "Effective network policy (profile %q, %s):\n", name, path)
	fmt.Fprintf(w, "  mode=%s  prompt=%s", p.Mode, onOff(p.PromptEnabled))
	if p.UpstreamProxy != "" {
		fmt.Fprintf(w, "  upstream_proxy=%s", p.UpstreamProxy)
	}
	fmt.Fprintf(w, "\n  allow_domain: %s\n", joinOrNone(p.AllowDomains))
	if len(p.DenyDomains) > 0 {
		fmt.Fprintf(w, "  deny_domain:  %s\n", joinOrNone(p.DenyDomains))
	}
	fmt.Fprintln(w, "  (full effective config across all subsystems: `omac provenance`)")
	fmt.Fprintln(w)
}

// reasonWord turns the audit source tokens into a short plain-language reason
// for the blocked-host line (the detail lives in the problem hint).
func reasonWord(sources []string) string {
	for _, s := range sources {
		switch s {
		case "blocklist":
			return "deny_domain"
		case "hard-deny":
			return "hard-deny"
		case "learned":
			return "learned deny"
		case "dns":
			return "DNS failed"
		case "unavailable":
			return "prompt unavailable"
		}
	}
	return "not allowed"
}

func joinOrNone(xs []string) string {
	if len(xs) == 0 {
		return "(none)"
	}
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

func hostPorts(host string, ports []int) string {
	if len(ports) == 0 {
		return host
	}
	out := host + ":"
	for i, p := range ports {
		if i > 0 {
			out += ","
		}
		out += strconv.Itoa(p)
	}
	return out
}
