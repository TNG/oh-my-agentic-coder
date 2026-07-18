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
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/diagnose"
	"github.com/tngtech/oh-my-agentic-coder/internal/netprompt"
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
	live := fs.Bool("live", false, "With --probe: if the static check says ALLOW, also attempt a real connection through the sandbox.")
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
		return runDiagnoseProbe(env, *profileRef, profile, profPath, *probe, *live, *asJSON)
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
		Version:    env.Version,
		OS:         osinfo.Detect().String(),
		Workdir:    env.Workdir,
		Profile:    profileSummary(profile, profPath),
		RunScope:   *runSel,
		RunID:      runID,
		AuditLog:   auditPath,
		SandboxLog: logPath,
		AuditNote:  auditNote,
		Report:     report,
	}

	if *asJSON {
		return writeDiagnoseJSON(env, view)
	}
	writeDiagnoseText(env, view)
	return ExitOK
}

// diagnoseView is the full result payload (text + JSON share it).
type diagnoseView struct {
	Version    string          `json:"version"`
	OS         string          `json:"os"`
	Workdir    string          `json:"workdir"`
	Profile    profileSummaryV `json:"profile"`
	RunScope   string          `json:"run_scope"`
	RunID      string          `json:"run_id,omitempty"`
	AuditLog   string          `json:"audit_log,omitempty"`
	SandboxLog string          `json:"sandbox_log,omitempty"`
	AuditNote  string          `json:"audit_note,omitempty"`
	Report     diagnose.Report `json:"report"`
}

type profileSummaryV struct {
	Name          string   `json:"name"`
	Path          string   `json:"path"`
	Mode          string   `json:"network_mode"`
	PromptEnabled bool     `json:"network_prompt_enabled"`
	UpstreamProxy string   `json:"upstream_proxy,omitempty"`
	AllowDomains  []string `json:"allow_domain,omitempty"`
	DenyDomains   []string `json:"deny_domain,omitempty"`
}

// policyFromProfile adapts the sandbox profile into the diagnose Policy
// DTO. Keeping this here (not in diagnose) is what decouples the analysis
// from the profile schema.
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

func profileSummary(p *sandboxprofile.Profile, path string) profileSummaryV {
	return profileSummaryV{
		Name:          p.Meta.Name,
		Path:          path,
		Mode:          p.Network.EffectiveMode(),
		PromptEnabled: p.Network.PromptEnabled(),
		UpstreamProxy: p.Network.UpstreamProxy,
		AllowDomains:  p.Network.AllowDomain,
		DenyDomains:   p.Network.DenyDomain,
	}
}

// decisionsFromEvents maps net.decision audit events to diagnose Decisions,
// optionally scoped to a single run. This adapter is the only place that
// knows the audit event schema.
func decisionsFromEvents(events []audit.Event, runID string) []diagnose.Decision {
	var out []diagnose.Decision
	for _, ev := range events {
		if ev.Type != audit.TypeNetDecision {
			continue
		}
		if runID != "" && ev.RunID != runID {
			continue
		}
		when, _ := time.Parse(time.RFC3339Nano, ev.Ts)
		out = append(out, diagnose.Decision{
			Host:    ev.Host,
			Port:    ev.Port,
			Allowed: ev.Allow != nil && *ev.Allow,
			Source:  ev.Source,
			When:    when,
		})
	}
	return out
}

// runDiagnoseProbe statically evaluates whether host[:port] would be
// admitted, reusing the real netproxy filter (no DNS, no dialing, no
// sandbox launch) so the verdict cannot drift from runtime behavior.
func runDiagnoseProbe(env *Env, profileRef string, profile *sandboxprofile.Profile, profPath, target string, live, asJSON bool) int {
	host, port, err := splitHostPortDefault(target, 443)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac diagnose: --probe:", err)
		return ExitMisuse
	}

	learned, _ := netprompt.LoadLearnedPolicy(sandboxprofile.PagesPath(profPath))
	// PromptEnabled is false here so the filter evaluates the static rules
	// only and never blocks on an interactive prompt; the "would prompt"
	// outcome is derived afterwards from the profile's real setting.
	f := netproxy.NewFilter(netproxy.FilterConfig{
		AllowDomains:  profile.Network.AllowDomain,
		DenyDomains:   profile.Network.DenyDomain,
		PromptEnabled: false,
		Learned:       learned,
	})
	outcome, reason := classifyProbe(f.CheckHost(host, port), profile.Network.PromptEnabled())

	pv := probeView{Host: host, Port: port, Outcome: string(outcome), Reason: reason}

	if live {
		if outcome != probeAllow {
			// Only allow-listed hosts are worth a live dial: a DENY is
			// already definitive, and an unlisted host would pop the
			// interactive prompt (or is denied by default).
			pv.Live = &probeResult{Target: target, Class: "skipped",
				Detail: fmt.Sprintf("static outcome is %s; not attempting a live connection", outcome)}
		} else if lr, lerr := runLiveProbe(env, profileRef, target); lerr != nil {
			pv.Live = &probeResult{Target: target, Class: "error", Detail: lerr.Error()}
		} else {
			pv.Live = lr
		}
	}

	if asJSON {
		return writeDiagnoseJSON(env, pv)
	}
	fmt.Fprintf(env.Stdout, "%s %s:%d (%s)\n", outcome, host, port, reason)
	if pv.Live != nil {
		fmt.Fprintf(env.Stdout, "live: %s (%s)\n", pv.Live.Class, pv.Live.Detail)
	}
	return ExitOK
}

// Live-probe timeouts. The child bounds its own HTTP request to
// liveProbeChildTimeout; liveProbeWallClock is a watchdog on the whole
// sandbox launch so the diagnose command can never hang, even if sandbox
// setup or teardown stalls. The child self-terminates on its own timeout
// regardless, so a watchdog trip abandons the wait rather than leaking a
// runaway process.
const (
	liveProbeChildTimeout = 12 * time.Second
	liveProbeWallClock    = 30 * time.Second
)

// runLiveProbe launches the real sandbox (via sandboxrun.Run) running omac's
// own hidden probe-connect command, which reaches the target through the
// injected proxy. The child writes its result to a temp file granted into
// the sandbox; this reads it back. Reusing sandboxrun.Run means the probe
// traverses the exact proxy + kernel-enforcement + upstream-chaining path a
// real session would. The caller only reaches here when the static check
// already returned ALLOW, so the probe can only contact a host the profile's
// own allowlist permits — it never dials a denied or unlisted host.
func runLiveProbe(env *Env, profileRef, target string) (*probeResult, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate omac binary: %w", err)
	}
	tmp, err := os.CreateTemp("", "omac-probe-*.json")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	flags := &sandboxprofile.Flags{
		ProfileRef: profileRef,
		AllowFile:  []string{tmpPath}, // grant the result file read+write inside the sandbox
		InnerArgv: []string{self, "sandbox", "probe-connect",
			"--out", tmpPath,
			"--timeout", strconv.Itoa(int(liveProbeChildTimeout.Seconds())),
			target},
	}

	done := make(chan int, 1)
	go func() {
		done <- sandboxrun.Run(sandboxrun.Options{Flags: flags, Workdir: env.Workdir, Stderr: env.Stderr})
	}()

	var code int
	select {
	case code = <-done:
	case <-time.After(liveProbeWallClock):
		return nil, fmt.Errorf("live probe exceeded %s and was abandoned (the sandboxed check self-terminates on its own %s timeout)",
			liveProbeWallClock, liveProbeChildTimeout)
	}

	data, rerr := os.ReadFile(tmpPath)
	if rerr != nil || len(data) == 0 {
		return nil, fmt.Errorf("the sandboxed probe produced no result (sandbox exit %d)", code)
	}
	var res probeResult
	if jerr := json.Unmarshal(data, &res); jerr != nil {
		return nil, fmt.Errorf("parse probe result: %w", jerr)
	}
	return &res, nil
}

type probeOutcome string

const (
	probeAllow  probeOutcome = "ALLOW"
	probeDeny   probeOutcome = "DENY"
	probePrompt probeOutcome = "PROMPT"
)

// classifyProbe turns a static filter verdict into the outcome a real run
// would produce. A verdict from an explicit rule (hard-deny/learned/
// deny_domain/allow_domain) is definitive; otherwise the request falls to
// the default path, where an enabled network prompt would ask the user
// interactively at runtime rather than silently allow/deny.
func classifyProbe(v netproxy.Verdict, promptEnabled bool) (probeOutcome, string) {
	definitive := strings.HasPrefix(v.Reason, "hard-deny") ||
		strings.HasPrefix(v.Reason, "learned") ||
		v.Reason == "deny_domain" || v.Reason == "allow_domain"
	if !definitive && promptEnabled {
		return probePrompt, "no static rule matches; the network prompt would ask interactively at runtime"
	}
	if v.Decision == netproxy.Allow {
		return probeAllow, v.Reason
	}
	return probeDeny, v.Reason
}

type probeView struct {
	Host    string       `json:"host"`
	Port    int          `json:"port"`
	Outcome string       `json:"outcome"`
	Reason  string       `json:"reason"`
	Live    *probeResult `json:"live,omitempty"`
}

func splitHostPortDefault(target string, defPort int) (string, int, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		// No port present: treat the whole string as the host.
		return target, defPort, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, port, nil
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

func writeDiagnoseText(env *Env, v diagnoseView) {
	w := env.Stdout
	fmt.Fprintf(w, "omac %s\n", v.Version)
	fmt.Fprintf(w, "OS: %s\n", v.OS)
	fmt.Fprintf(w, "workdir: %s\n\n", v.Workdir)

	fmt.Fprintf(w, "Effective network policy (profile %q, %s):\n", v.Profile.Name, v.Profile.Path)
	fmt.Fprintf(w, "  mode=%s  prompt=%s", v.Profile.Mode, onOff(v.Profile.PromptEnabled))
	if v.Profile.UpstreamProxy != "" {
		fmt.Fprintf(w, "  upstream_proxy=%s", v.Profile.UpstreamProxy)
	}
	fmt.Fprintf(w, "\n  allow_domain: %s\n", joinOrNone(v.Profile.AllowDomains))
	if len(v.Profile.DenyDomains) > 0 {
		fmt.Fprintf(w, "  deny_domain:  %s\n", joinOrNone(v.Profile.DenyDomains))
	}
	fmt.Fprintln(w, "  (full effective config across all subsystems: `omac provenance`)")
	fmt.Fprintln(w)

	if v.AuditNote != "" {
		fmt.Fprintf(w, "[warn] %s\n\n", v.AuditNote)
	}

	scope := "most recent run"
	if v.RunScope == "all" {
		scope = "all runs"
	}
	fmt.Fprintf(w, "Network decisions (%s): %d total, %d denied\n", scope, v.Report.Total, v.Report.Denied)
	if len(v.Report.Blocked) == 0 {
		fmt.Fprintln(w, "  no blocked connections recorded.")
	} else {
		fmt.Fprintln(w, "  blocked hosts (most-blocked first):")
		for _, b := range v.Report.Blocked {
			fmt.Fprintf(w, "    %4dx  DENY %-40s (%s)\n", b.Count, hostPorts(b.Host, b.Ports), joinOrNone(b.Sources))
		}
	}
	fmt.Fprintln(w)

	if len(v.Report.Hints) > 0 {
		fmt.Fprintln(w, "Hints:")
		for _, h := range v.Report.Hints {
			fmt.Fprintf(w, "  [%s] %s\n", h.Severity, h.Title)
			for _, d := range h.Detail {
				fmt.Fprintf(w, "         %s\n", d)
			}
		}
		fmt.Fprintln(w)
	}

	if v.AuditLog != "" {
		fmt.Fprintf(w, "audit trail: %s\n", v.AuditLog)
	}
	if v.SandboxLog != "" {
		fmt.Fprintf(w, "raw sandbox log (TTY sessions): %s\n", v.SandboxLog)
	}
	fmt.Fprintln(w, "probe a host without running:  omac diagnose --probe <host>")
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
