package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// probeResult is the machine-readable outcome of a live connectivity probe.
// It is written by the sandboxed `omac sandbox probe-connect` child and read
// back by `omac diagnose --probe --live`.
type probeResult struct {
	Target  string `json:"target"`
	Reached bool   `json:"reached"` // the network path worked (proxy allowed + TCP reached the destination)
	Class   string `json:"class"`   // reached|blocked|dns|refused|timeout|tls|skipped|error
	Detail  string `json:"detail"`
}

// runSandboxProbeConnect is the hidden inner command the live probe runs
// inside the sandbox. It attempts to reach the target through the injected
// HTTP(S)_PROXY (the real omac proxy) and records the outcome to --out. It
// is not a user-facing command; `omac diagnose --probe --live` invokes it.
func runSandboxProbeConnect(args []string, env *Env) int {
	fs := flag.NewFlagSet("probe-connect", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	out := fs.String("out", "", "write the JSON result to this file")
	timeoutSecs := fs.Int("timeout", 15, "connection timeout in seconds")
	if err := fs.Parse(args); err != nil {
		return ExitMisuse
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(env.Stderr, "usage: omac sandbox probe-connect [--out <file>] [--timeout <secs>] <host[:port]>")
		return ExitMisuse
	}
	res := probeConnect(fs.Arg(0), time.Duration(*timeoutSecs)*time.Second)
	if *out != "" {
		// Result goes to the file only; keeping stdout clean lets the
		// parent emit valid --json.
		if data, err := json.Marshal(res); err == nil {
			_ = os.WriteFile(*out, data, 0o644)
		}
	} else {
		fmt.Fprintf(env.Stdout, "probe %s: %s (%s)\n", res.Target, res.Class, res.Detail)
	}
	return ExitOK
}

// probeConnect makes a real request to the target through the ambient proxy
// environment and classifies the outcome. It measures reachability of the
// network path: a destination TLS error still counts as "reached" (the proxy
// tunnel got to the host), so certificate verification is left on — there is
// no need to weaken it.
func probeConnect(target string, timeout time.Duration) probeResult {
	host, port, err := splitHostPortDefault(target, 443)
	if err != nil {
		return probeResult{Target: target, Class: "error", Detail: err.Error()}
	}
	url := fmt.Sprintf("https://%s:%d/", host, port)
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodHead, url, nil)
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
		return probeResult{Target: target, Reached: true, Class: "reached",
			Detail: fmt.Sprintf("HTTP %d via proxy", resp.StatusCode)}
	}
	return classifyProbeError(target, err)
}

// classifyProbeError maps a transport error to a probe class. The strings
// come from net/http and the omac proxy's deny response; the fallback keeps
// the raw message so an unclassified failure is still informative.
func classifyProbeError(target string, err error) probeResult {
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "denied") || strings.Contains(low, "403"):
		return probeResult{Target: target, Class: "blocked", Detail: "proxy denied the connection (sandbox network policy)"}
	case strings.Contains(low, "no such host") || strings.Contains(low, "dns"):
		return probeResult{Target: target, Class: "dns", Detail: "DNS resolution failed"}
	case strings.Contains(low, "refused"):
		return probeResult{Target: target, Class: "refused", Detail: "connection refused by the destination"}
	case strings.Contains(low, "timeout") || strings.Contains(low, "deadline"):
		return probeResult{Target: target, Class: "timeout", Detail: "timed out"}
	case strings.Contains(low, "tls") || strings.Contains(low, "certificate") || strings.Contains(low, "handshake"):
		// The proxy tunnel reached the destination; only the destination
		// TLS layer failed — the network path itself works.
		return probeResult{Target: target, Reached: true, Class: "tls", Detail: "connected; destination TLS issue: " + err.Error()}
	default:
		return probeResult{Target: target, Class: "error", Detail: err.Error()}
	}
}
