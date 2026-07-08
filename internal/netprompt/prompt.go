package netprompt

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/netproxy"
)

// Decision tokens, mirroring nono's parse_decision_token.
const (
	tokenAllowOnce            = "allow_once"
	tokenAllowPermanentHost   = "allow_permanent_host"
	tokenAllowPermanentSuffix = "allow_permanent_suffix"
	tokenDenyOnce             = "deny_once"
	tokenDenyPermanentHost    = "deny_permanent_host"
	tokenDenyPermanentSuffix  = "deny_permanent_suffix"
	tokenNeedsIntent          = "needs_intent"
)

// optionLabels are the exact seven dialog choices (nono parity, product
// name swapped). Order matters: Deny once is the default.
func optionLabels(suffix string) []string {
	return []string{
		"Allow once",
		"Allow permanently (this host)",
		fmt.Sprintf("Allow permanently (*.%s)", suffix),
		"Deny once",
		"Deny permanently (this host)",
		fmt.Sprintf("Deny permanently (*.%s)", suffix),
		"Explain more",
	}
}

// labelToToken maps a chosen label back to a decision token.
func labelToToken(label, suffix string) string {
	switch label {
	case "Allow once":
		return tokenAllowOnce
	case "Allow permanently (this host)":
		return tokenAllowPermanentHost
	case fmt.Sprintf("Allow permanently (*.%s)", suffix):
		return tokenAllowPermanentSuffix
	case "Deny permanently (this host)":
		return tokenDenyPermanentHost
	case fmt.Sprintf("Deny permanently (*.%s)", suffix):
		return tokenDenyPermanentSuffix
	case "Explain more":
		return tokenNeedsIntent
	default:
		return tokenDenyOnce
	}
}

// tokenToResult converts a decision token into a netproxy.PromptResult.
func tokenToResult(token, host, suffix string) netproxy.PromptResult {
	switch token {
	case tokenAllowOnce:
		return netproxy.PromptResult{Allow: true}
	case tokenAllowPermanentHost:
		return netproxy.PromptResult{Allow: true, Persist: true, Scope: "host"}
	case tokenAllowPermanentSuffix:
		return netproxy.PromptResult{Allow: true, Persist: true, Scope: "suffix", Suffix: suffix}
	case tokenDenyPermanentHost:
		return netproxy.PromptResult{Allow: false, Persist: true, Scope: "host"}
	case tokenDenyPermanentSuffix:
		return netproxy.PromptResult{Allow: false, Persist: true, Scope: "suffix", Suffix: suffix}
	case tokenNeedsIntent:
		return netproxy.PromptResult{Allow: false, NeedsIntent: true}
	default: // deny_once and anything unparseable
		return netproxy.PromptResult{Allow: false}
	}
}

// RegisteredSuffixHint strips the leftmost label of a host with >= 3
// labels (api.example.com -> example.com). IP literals and shorter
// hosts are returned unchanged (nono's registered_suffix_hint).
func RegisteredSuffixHint(host string) string {
	if net.ParseIP(host) != nil {
		return host
	}
	labels := strings.Split(host, ".")
	if len(labels) >= 3 {
		return strings.Join(labels[1:], ".")
	}
	return host
}

// promptText is the dialog body. urlPath is the request path when known
// (forward HTTP only; CONNECT can't see it). intent is the agent-declared
// reason; empty means "not declared".
func promptText(host string, port int, urlPath, intent string) string {
	target := fmt.Sprintf("%s:%d", host, port)
	if urlPath != "" {
		target = fmt.Sprintf("https://%s:%d%s", host, port, urlPath)
	}
	intentLine := "Agent intent: (not declared)"
	if intent != "" {
		intentLine = fmt.Sprintf("Agent intent: %q", intent)
	}
	return fmt.Sprintf("The sandboxed process is trying to reach:\n\n    %s\n\n%s\n\nHow should omac handle this destination?", target, intentLine)
}

// notificationText is the parallel OS notification body.
func notificationText(host string, port int) string {
	return fmt.Sprintf("Sandboxed process wants to reach %s:%d — a decision dialog is waiting.", host, port)
}

// dialogBackend runs one dialog tool and returns the chosen label.
type dialogBackend interface {
	name() string
	available() bool
	// show blocks until the user chooses, the dialog is cancelled, or
	// ctx is done (the implementation must kill the dialog process).
	// Returns the chosen label ("" on cancel).
	show(ctx context.Context, host string, port int, suffix, urlPath, intent string) (string, error)
}

// Prompter implements netproxy.Prompter with native dialogs.
type Prompter struct {
	timeout      time.Duration
	backends     []dialogBackend
	notify       func(host string, port int)
	logf         func(format string, args ...any)
	lookupIntent func(host string) (string, bool)
}

// NewPrompter builds the platform prompter. Returns the prompter and
// whether any dialog backend is available (callers feed that into the
// on_unavailable policy). lookupIntent, when non-nil, supplies the
// agent-declared reason for the host; nil = no registry.
func NewPrompter(timeoutSecs int, logf func(string, ...any), lookupIntent func(host string) (string, bool)) (*Prompter, bool) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := &Prompter{
		timeout:      time.Duration(timeoutSecs) * time.Second,
		logf:         logf,
		lookupIntent: lookupIntent,
	}
	if runtime.GOOS == "darwin" {
		p.backends = []dialogBackend{osascriptBackend{}}
		p.notify = notifyDarwin
	} else {
		p.backends = []dialogBackend{zenityBackend{}, kdialogBackend{}}
		p.notify = notifyLinux
	}
	for _, b := range p.backends {
		if b.available() {
			return p, true
		}
	}
	return p, false
}

// Prompt implements netproxy.Prompter. Timeout or any dialog failure
// returns deny-once-shaped "unavailable" semantics: the filter layer
// has already decided what on_unavailable means, so here we encode
// failure as deny (the conservative default) — except the caller
// (run.go) wires OnUnavailableAllow into the filter, which handles the
// no-backend case before Prompt is ever called. A timed-out dialog is
// always deny (nono: "a missed prompt never silently allows").
func (p *Prompter) Prompt(host string, port int) netproxy.PromptResult {
	var backend dialogBackend
	for _, b := range p.backends {
		if b.available() {
			backend = b
			break
		}
	}
	if backend == nil {
		return netproxy.PromptResult{Allow: false}
	}
	if p.notify != nil {
		go p.notify(host, port)
	}
	suffix := RegisteredSuffixHint(host)
	intent := ""
	if p.lookupIntent != nil {
		if reason, ok := p.lookupIntent(host); ok {
			intent = reason
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	label, err := backend.show(ctx, host, port, suffix, "", intent)
	if err != nil {
		if ctx.Err() != nil {
			p.logf("omac sandbox: network prompt for %s:%d timed out", host, port)
		} else {
			p.logf("omac sandbox: network prompt failed: %v", err)
		}
		return netproxy.PromptResult{Allow: false}
	}
	token := labelToToken(label, suffix)
	return tokenToResult(token, host, suffix)
}

// --- macOS ---

type osascriptBackend struct{}

func (osascriptBackend) name() string { return "osascript" }

func (osascriptBackend) available() bool {
	_, err := exec.LookPath("osascript")
	return err == nil
}

func (osascriptBackend) show(ctx context.Context, host string, port int, suffix, urlPath, intent string) (string, error) {
	opts := optionLabels(suffix)
	quoted := make([]string, len(opts))
	for i, o := range opts {
		quoted[i] = appleScriptString(o)
	}
	script := fmt.Sprintf(
		`choose from list {%s} with title "omac: network access" with prompt %s default items {%s} OK button name "Select" cancel button name "Cancel"`,
		strings.Join(quoted, ", "),
		appleScriptString(promptText(host, port, urlPath, intent)),
		appleScriptString("Deny once"),
	)
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	if err != nil {
		return "", err
	}
	choice := strings.TrimSpace(string(out))
	if choice == "false" || choice == "" { // Cancel
		return "", nil
	}
	return choice, nil
}

// appleScriptString quotes s as an AppleScript string literal.
func appleScriptString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

func notifyDarwin(host string, port int) {
	script := fmt.Sprintf(`display notification %s with title "omac: network request"`,
		appleScriptString(notificationText(host, port)))
	_ = exec.Command("osascript", "-e", script).Run()
}

// --- Linux ---

type zenityBackend struct{}

func (zenityBackend) name() string { return "zenity" }

func (zenityBackend) available() bool {
	_, err := exec.LookPath("zenity")
	return err == nil
}

func (zenityBackend) show(ctx context.Context, host string, port int, suffix, urlPath, intent string) (string, error) {
	args := []string{
		"--list", "--radiolist",
		"--title", "omac: network access",
		"--text", promptText(host, port, urlPath, intent),
		"--column", "", "--column", "Decision",
		"--height", "320",
	}
	for _, o := range optionLabels(suffix) {
		sel := "FALSE"
		if o == "Deny once" {
			sel = "TRUE"
		}
		args = append(args, sel, o)
	}
	out, err := exec.CommandContext(ctx, "zenity", args...).Output()
	if err != nil {
		// zenity exits 1 on Cancel; treat as cancel unless ctx expired.
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

type kdialogBackend struct{}

func (kdialogBackend) name() string { return "kdialog" }

func (kdialogBackend) available() bool {
	_, err := exec.LookPath("kdialog")
	return err == nil
}

func (kdialogBackend) show(ctx context.Context, host string, port int, suffix, urlPath, intent string) (string, error) {
	opts := optionLabels(suffix)
	args := []string{
		"--title", "omac: network access",
		"--radiolist", promptText(host, port, urlPath, intent),
	}
	for i, o := range opts {
		state := "off"
		if o == "Deny once" {
			state = "on"
		}
		args = append(args, fmt.Sprintf("%d", i), o, state)
	}
	out, err := exec.CommandContext(ctx, "kdialog", args...).Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", nil
	}
	idx := strings.TrimSpace(string(out))
	for i, o := range opts {
		if fmt.Sprintf("%d", i) == idx {
			return o, nil
		}
	}
	return "", nil
}

func notifyLinux(host string, port int) {
	if _, err := exec.LookPath("notify-send"); err == nil {
		_ = exec.Command("notify-send", "omac: network request", notificationText(host, port)).Run()
	}
}
