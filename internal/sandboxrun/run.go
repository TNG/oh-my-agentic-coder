package sandboxrun

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/intent"
	"github.com/tngtech/oh-my-agentic-coder/internal/netprompt"
	"github.com/tngtech/oh-my-agentic-coder/internal/netproxy"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandbox"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxdeny"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

// Options bundles the inputs for Run.
type Options struct {
	Flags   *sandboxprofile.Flags
	Workdir string
	Stderr  io.Writer
}

// Run is the `omac sandbox run` supervisor: resolve profile + flags,
// start the filtering proxy, launch the sandboxed child, forward
// signals, propagate the exit code, tear everything down. Returns the
// process exit code.
func Run(opts Options) int {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	// Fatal/pre-launch problems go to stderr (the TUI hasn't started
	// yet). Runtime diagnostics — proxy decisions, prompt notices —
	// fire while the inner TUI owns the terminal, so they go through
	// the diag sink (a log file when stderr is a terminal).
	diag := newDiagSink(stderr)
	defer diag.Close()

	fail := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "omac sandbox: "+format+"\n", args...)
		return 1
	}

	profile, profilePath, err := sandboxprofile.Resolve(opts.Flags.ProfileRef)
	if err != nil {
		return fail("%v", err)
	}
	merged, warnings := sandboxprofile.Merge(profile, opts.Flags)
	for _, w := range warnings {
		fmt.Fprintf(stderr, "omac sandbox: warning: %s\n", w)
	}
	if err := merged.Validate(); err != nil {
		return fail("%v", err)
	}

	grants, err := ResolveGrants(merged, opts.Workdir, diag.Writer())
	if err != nil {
		return fail("%v", err)
	}
	if err := grants.Validate(); err != nil {
		return fail("%v", err)
	}
	den := resolvedDenial(merged.Denial)
	grants.DenialText = den.MarkerFile
	grants.DenialDirName = den.MarkerDirName

	// Intent lookup: the agent declares intents via POST $OMAC_BASE/
	// /sandbox/intent (the facade, in the parent process). The popup
	// and learn-mode review look them up over HTTP — the facade owns
	// the registry, not this process.
	intentBase := os.Getenv("OMAC_BASE")
	logf := diag.Logf
	if intentBase == "" {
		logf("omac sandbox: OMAC_BASE unset — intent lookup disabled, popups will show (not declared)")
	}

	cacheEnv, err := injectedToolCacheEnv(grants, os.Getenv)
	if err != nil {
		return fail("%v", err)
	}

	// Learn mode: lift filesystem restrictions (network/env filtering
	// stay active) and record the folders the session touches. The
	// recorder's exclusion sets are built from the *restricted* grants
	// so already-granted folders are never offered.
	var recorder *learnRecorder
	if opts.Flags.Learn {
		fmt.Fprintln(stderr, "omac sandbox: LEARN MODE — filesystem access is unrestricted this session; folders used will be offered for the profile at exit")
		recorder = newLearnRecorder(grants, intentBase)
		grants = grants.withUnrestrictedFilesystem()
	}

	// Injected child env. The validated cache redirect is recreated here
	// and proxy vars are added before the backend builds its rules.
	injected := cacheEnv

	// Audit sink for network decisions. This subprocess is separate from
	// the parent omac, so it opens its own append-only handle to the same
	// persistent audit file (append-safe across processes). Non-strict
	// here: a net-decision audit write failure must never kill the sandbox
	// (strict is enforced by the parent's pre-launch probe). Disabled when
	// no --audit-log path was passed down.
	netAuditor := audit.Nop()
	if opts.Flags.AuditLog != "" {
		mode := audit.ModeStart
		if opts.Flags.AuditMode == string(audit.ModeServe) {
			mode = audit.ModeServe
		}
		if a, aerr := audit.New(audit.Config{
			Enabled: true, Path: opts.Flags.AuditLog, Mode: mode, Strict: false,
			RunID: opts.Flags.AuditRunID,
		}); aerr != nil {
			fmt.Fprintf(stderr, "omac sandbox: warning: audit log unavailable (%v)\n", aerr)
		} else {
			netAuditor = a
			defer netAuditor.Close()
		}
	}

	var proxy *netproxy.Server
	if grants.NetworkMode == sandboxprofile.ModeFiltered {
		if grants.Enforcement == sandboxprofile.EnforceEnvOnly {
			fmt.Fprintln(stderr, "omac sandbox: WARNING: network.enforcement is \"env-only\" — "+
				"filtering relies on HTTP(S)_PROXY env vars only and is trivially bypassable. "+
				"No kernel network guarantee is in effect.")
		}
		proxy, err = buildProxy(merged, profilePath, diag.Writer(), logf, netAuditor, intentBase)
		if err != nil {
			return fail("%v", err)
		}
		defer proxy.Close()
		grants.ProxyPort = proxy.Port()
		for k, v := range proxy.EnvVars() {
			injected[k] = v
		}
	}

	// Denial markers must outlive argv construction: bwrap reads the
	// bind sources at launch, so cleanup is deferred until after the
	// child exits (below), not when BuildChildArgv returns.
	markerCleanup, err := grants.prepareMarkers()
	if err != nil {
		return fail("prepare denial markers: %v", err)
	}
	defer markerCleanup()

	childArgv, err := BuildChildArgv(grants, opts.Flags.InnerArgv)
	if err != nil {
		return fail("%v", err)
	}

	// Last line before the inner process owns the terminal: tell the
	// user where runtime diagnostics will land.
	diag.AnnouncePath()

	env := sandboxprofile.FilterEnv(os.Environ(), merged.Environment.AllowVars, injected)
	var onReady func(int)
	if recorder != nil {
		onReady = recorder.Start
	}
	code, err := sandbox.ExecWithEnv(childArgv, env, onReady)
	if err != nil {
		return fail("%v", err)
	}

	if recorder != nil {
		candidates := recorder.Stop()
		if oerr := OfferLearnedFolders(profilePath, candidates, os.Stdin, stderr, intentBase); oerr != nil {
			fmt.Fprintf(stderr, "omac sandbox: %v\n", oerr)
		}
	}
	return code
}

// injectedToolCacheEnv recreates the cache redirects for a sandbox re-exec
// only after the outer launcher-provided cache directory is an exact writable
// grant. This prevents inherited tool-specific cache paths from bypassing the
// profile's environment allowlist.
func injectedToolCacheEnv(grants *Grants, getenv func(string) string) (map[string]string, error) {
	dir := getenv("OMAC_CACHE_DIR")
	if dir == "" {
		return map[string]string{}, nil
	}

	var mode toolcache.Mode
	switch getenv("OMAC_CACHE_MODE") {
	case string(toolcache.ModePersistent):
		mode = toolcache.ModePersistent
	case string(toolcache.ModeEphemeral):
		mode = toolcache.ModeEphemeral
	default:
		return nil, fmt.Errorf("invalid OMAC_CACHE_MODE %q", getenv("OMAC_CACHE_MODE"))
	}

	cacheDir, err := canonicalCachePath(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve OMAC_CACHE_DIR: %w", err)
	}
	info, err := os.Stat(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("stat OMAC_CACHE_DIR: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("OMAC_CACHE_DIR %q is not a directory", dir)
	}
	for _, allowed := range grants.AllowPaths {
		allowedDir, err := canonicalCachePath(allowed)
		if err != nil {
			return nil, fmt.Errorf("resolve writable grant %q: %w", allowed, err)
		}
		if cacheDir == allowedDir {
			return toolcache.Environment(cacheDir, mode), nil
		}
	}
	return nil, fmt.Errorf("OMAC_CACHE_DIR %q does not match an exact writable grant", dir)
}

// canonicalCachePath compares cleaned absolute paths and resolves symlinks for
// existing entries, matching the paths that the platform backends enforce.
func canonicalCachePath(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(abs); err == nil {
		return filepath.EvalSymlinks(abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return abs, nil
}

// buildProxy assembles page policy, prompter, filter and server. The
// page policy (learned website decisions) lives next to the profile:
// <profile>.pages.json (e.g. default.pages.json).
func buildProxy(p *sandboxprofile.Profile, profilePath string, stderr io.Writer, logf func(string, ...any), auditor audit.Auditor, intentBase string) (*netproxy.Server, error) {
	var learned netproxy.LearnedStore
	pagesPath := sandboxprofile.PagesPath(profilePath)
	lp, lerr := netprompt.LoadLearnedPolicy(pagesPath)
	if lerr != nil {
		fmt.Fprintf(stderr, "omac sandbox: warning: %v (starting with empty page policy)\n", lerr)
		lp, _ = netprompt.LoadLearnedPolicy("")
	}
	learned = lp

	var prompter netproxy.Prompter
	onUnavailableAllow := p.Network.OnUnavailable() == sandboxprofile.OnUnavailableAllow
	if p.Network.PromptEnabled() {
		np, available := netprompt.NewPrompter(p.Network.PromptTimeoutSecs(), logf, func(host string) (string, bool) {
			return intent.LookupOverHTTP(intentBase, host)
		}, func(host string) {
			intent.MarkExplainMoreOverHTTP(intentBase, host)
		})
		if available {
			prompter = np
		} else {
			fmt.Fprintf(stderr, "omac sandbox: notice: no dialog backend available; network prompt falls back to on_unavailable=%s\n",
				p.Network.OnUnavailable())
		}
	}

	filter := netproxy.NewFilter(netproxy.FilterConfig{
		AllowDomains:       p.Network.AllowDomain,
		DenyDomains:        p.Network.DenyDomain,
		PromptEnabled:      p.Network.PromptEnabled(),
		OnUnavailableAllow: onUnavailableAllow,
		Prompter:           prompter,
		Learned:            learned,
		Logf:               logf,
		Auditor:            auditor,
	})
	srv, err := netproxy.NewServer(filter, logf)
	if err != nil {
		return nil, err
	}
	if err := srv.Start(); err != nil {
		return nil, err
	}
	return srv, nil
}

// resolvedDenial merges a profile's Denial override with the compiled-in
// default and returns the resolved text (marker file + dir-notice name).
// Empty override fields inherit the default.
func resolvedDenial(d *sandboxprofile.Denial) sandboxdeny.Text {
	if d == nil {
		return sandboxdeny.Default()
	}
	return sandboxdeny.Resolve(sandboxdeny.Text{
		MarkerFile:    d.MarkerFile,
		MarkerDirName: d.MarkerDirName,
		FacadeNote:    d.FacadeNote,
	})
}
