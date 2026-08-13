package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
	"github.com/tngtech/oh-my-agentic-coder/internal/credproxy"
)

// cliProxyStarter is the production ProxyStarter adapter: it wires the
// engine's ProxyStarter seam to the existing cli startBuildProxy /
// startCredentialProxy / startContainerProxy functions, preserving the
// documented startup ORDERING (filtered → credential → container) and
// the deferred cleanup chain. The engine owns the defer chain for
// cleanup; this adapter only starts the proxies and returns stop funcs.
//
// Nothing in the adapter exposes proxy constructors, daemon endpoints,
// credential values, or host-policy internals to the engine — the
// engine consumes only URLs/enabled flags/stop funcs.
//
// A missing keychain credential for an approved private registry is a
// *credproxy.RegistryCredentialError — the adapter surfaces it as a
// policy denial (criterion 7, exit 3) by wrapping it with
// buildengine.ErrPolicyDenial so the engine maps the result class
// correctly. The credential itself NEVER enters the error or the
// engine.
func cliProxyStarter(env *buildengine.ProxyEnv) (filtered buildengine.ProxyHandle, credential buildengine.CredentialProxyHandle, container buildengine.ContainerProxyHandle, err error) {
	cliEnv := &Env{
		Workdir: env.Workdir,
		Stderr:  stderrFileFor(env.Stderr),
		// TraceWriter carries the broker's io.Writer stderr (chunked
		// back to the inner omac build client) so proxy seams that
		// log via io.Writer (not *os.File) still reach the captured
		// output. stderrFileFor returns nil for the chunked writer,
		// which silently drops every containerproxy log line in the
		// brokered path; TraceWriter is the escape hatch. Falls back
		// to os.Stderr when env.Stderr is nil (direct-host path keeps
		// using the process *os.File).
		TraceWriter: env.Stderr,
	}
	if os.Getenv("OMAC_BUILD_TRACE") == "1" {
		fmt.Fprintf(cliEnv.traceWriter(), "omac build: cliProxyStarter: workdir=%s worktree=%s leaf=%q approvedImages=%v approvedRegistries=%v buildReqID=%s\n",
			env.Workdir, env.Worktree, env.Leaf, env.ApprovedImages, env.ApprovedRegistries, env.BuildRequestID)
	}

	// 1. Filtered proxy (macOS v1; Linux kernel-blocked → not started).
	proxyURL, proxyPort, stopProxy, proxyErr := startBuildProxy(cliEnv)
	if proxyErr != nil {
		return buildengine.ProxyHandle{}, buildengine.CredentialProxyHandle{}, buildengine.ContainerProxyHandle{}, proxyErr
	}
	filtered = buildengine.ProxyHandle{
		URL:     proxyURL,
		Port:    proxyPort,
		Enabled: stopProxy != nil,
		Stop:    stopProxy,
	}

	// 2. Credential-lift proxy. A missing credential for an approved
	// private registry is a fail-closed policy denial on EVERY platform
	// (criterion 7); the lookup runs before the macOS-only server
	// gate. The adapter surfaces a *RegistryCredentialError as
	// ErrPolicyDenial so the engine maps it to ClassPolicyDenial (exit
	// 3), NOT ClassServiceFailure (exit 10). The filtered proxy stop
	// func is returned to the engine regardless, so its defer chain
	// tears it down on the denial path.
	credURLs, stopCredProxy, credErr := startCredentialProxy(cliEnv, env.Worktree, env.Leaf, env.ManifestRegistries, env.ApprovedRegistries)
	if credErr != nil {
		var regErr *credproxy.RegistryCredentialError
		if errors.As(credErr, &regErr) {
			return filtered, buildengine.CredentialProxyHandle{}, buildengine.ContainerProxyHandle{},
				fmt.Errorf("%w: %v", buildengine.ErrPolicyDenial, credErr)
		}
		return filtered, buildengine.CredentialProxyHandle{}, buildengine.ContainerProxyHandle{},
			fmt.Errorf("credential proxy: %w", credErr)
	}
	credential = buildengine.CredentialProxyHandle{
		URLs: credURLs,
		Stop: stopCredProxy,
	}

	// 3. Container proxy (macOS v1 with approved images; Linux
	// kernel-blocked → not started). The build request id is threaded
	// in so container-policy denials are correlated with the active
	// request (spec §254).
	containerURL, containerEnabled, stopContainerProxy, cpErr := containerProxyStarter(cliEnv, env.Worktree, env.Leaf, env.ApprovedImages, env.BuildRequestID, env.Auditor)
	if cpErr != nil {
		return filtered, credential, buildengine.ContainerProxyHandle{},
			fmt.Errorf("container proxy: %w", cpErr)
	}
	container = buildengine.ContainerProxyHandle{
		URL:     containerURL,
		Enabled: containerEnabled,
		Stop:    stopContainerProxy,
	}
	return filtered, credential, container, nil
}

// stderrFileFor returns the *os.File backing the engine's io.Writer
// stderr, or nil when the writer is not an *os.File. The engine accepts
// io.Writer (transport-neutral); the cli proxy seams write to env.Stderr
// (*os.File) via fmt.Fprintf and nil-check it before writing. In
// production the engine's stderr is the process's *os.File stderr; in
// tests it may be a temp file. Returning nil for non-*os.File writers is
// safe — the proxy seams skip logging when env.Stderr is nil.
//
// This keeps the engine free of an *os.File dependency while letting the
// existing cli proxy seams keep their *Env.Stderr signature unchanged
// (the prefactor does not rewrite the proxy seams — they stay as the
// lower-level module wiring they already are).
func stderrFileFor(w interface{ Write([]byte) (int, error) }) *os.File {
	if w == nil {
		return nil
	}
	if f, ok := w.(*os.File); ok {
		return f
	}
	return nil
}
