package sandboxrun

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/netprompt"
	"github.com/tngtech/oh-my-agentic-coder/internal/netproxy"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandbox"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
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

	// Learn mode: lift filesystem restrictions (network/env filtering
	// stay active) and record the folders the session touches. The
	// recorder's exclusion sets are built from the *restricted* grants
	// so already-granted folders are never offered.
	var recorder *learnRecorder
	if opts.Flags.Learn {
		fmt.Fprintln(stderr, "omac sandbox: LEARN MODE — filesystem access is unrestricted this session; folders used will be offered for the profile at exit")
		recorder = newLearnRecorder(grants)
		grants = grants.withUnrestrictedFilesystem()
	}

	logf := diag.Logf

	// Injected child env (proxy vars). Built before the backend so the
	// proxy port can land in the kernel rules.
	injected := map[string]string{}

	var proxy *netproxy.Server
	if grants.NetworkMode == sandboxprofile.ModeFiltered {
		if grants.Enforcement == sandboxprofile.EnforceEnvOnly {
			fmt.Fprintln(stderr, "omac sandbox: WARNING: network.enforcement is \"env-only\" — "+
				"filtering relies on HTTP(S)_PROXY env vars only and is trivially bypassable. "+
				"No kernel network guarantee is in effect.")
		}
		proxy, err = buildProxy(merged, profilePath, diag.Writer(), logf)
		if err != nil {
			return fail("%v", err)
		}
		defer proxy.Close()
		grants.ProxyPort = proxy.Port()
		for k, v := range proxy.EnvVars() {
			injected[k] = v
		}
	}

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
		if oerr := OfferLearnedFolders(profilePath, candidates, os.Stdin, stderr); oerr != nil {
			fmt.Fprintf(stderr, "omac sandbox: %v\n", oerr)
		}
	}
	return code
}

// buildProxy assembles page policy, prompter, filter and server. The
// page policy (learned website decisions) lives next to the profile:
// <profile>.pages.json (e.g. default.pages.json).
func buildProxy(p *sandboxprofile.Profile, profilePath string, stderr io.Writer, logf func(string, ...any)) (*netproxy.Server, error) {
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
		np, available := netprompt.NewPrompter(p.Network.PromptTimeoutSecs(), logf)
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
	})
	srv, err := netproxy.NewServer(filter, resolveUpstreamProxy(p, filter, stderr, logf), logf)
	if err != nil {
		return nil, err
	}
	if err := srv.Start(); err != nil {
		return nil, err
	}
	return srv, nil
}

// resolveUpstreamProxy resolves the upstream proxy URL and NO_PROXY list
// from the profile first, then the host environment, and returns the
// appropriate Dialer. If no upstream proxy is configured, it returns a
// direct dialer. An invalid proxy URL is a soft warning: log and fall
// back to direct dialing (the proxy URL may be wrong but the sandbox
// should still start; per-request upstream errors are deferred to 502s).
//
// Resolution order for the proxy URL:
//   - profile network.upstream_proxy
//   - env HTTPS_PROXY / https_proxy
//   - env HTTP_PROXY  / http_proxy
//
// Resolution order for NO_PROXY:
//   - profile network.no_proxy (if non-empty)
//   - env NO_PROXY / no_proxy (comma-separated, trimmed)
//
// Only proxyURL.Host is logged — never the userinfo (credentials).
func resolveUpstreamProxy(p *sandboxprofile.Profile, filter *netproxy.Filter, stderr io.Writer, logf func(string, ...any)) netproxy.Dialer {
	proxyStr := p.Network.UpstreamProxy
	if proxyStr == "" {
		proxyStr = os.Getenv("HTTPS_PROXY")
	}
	if proxyStr == "" {
		proxyStr = os.Getenv("https_proxy")
	}
	if proxyStr == "" {
		proxyStr = os.Getenv("HTTP_PROXY")
	}
	if proxyStr == "" {
		proxyStr = os.Getenv("http_proxy")
	}

	if proxyStr == "" {
		return netproxy.NewDirectDialer(filter)
	}

	proxyURL, err := url.Parse(proxyStr)
	if err != nil {
		fmt.Fprintf(stderr, "omac sandbox: warning: invalid upstream proxy %q: %v — falling back to direct dialing\n", proxyStr, err)
		return netproxy.NewDirectDialer(filter)
	}

	var noProxy []string
	if len(p.Network.NoProxy) > 0 {
		noProxy = p.Network.NoProxy
	} else {
		noProxyStr := os.Getenv("NO_PROXY")
		if noProxyStr == "" {
			noProxyStr = os.Getenv("no_proxy")
		}
		if noProxyStr != "" {
			noProxy = strings.Split(noProxyStr, ",")
			for i, s := range noProxy {
				noProxy[i] = strings.TrimSpace(s)
			}
		}
	}

	logf("omac sandbox: using upstream proxy %s", proxyURL.Host)

	return netproxy.NewUpstreamProxyDialer(proxyURL, noProxy, filter, logf)
}
