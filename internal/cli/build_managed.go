package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildbroker"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// Managed-mode environment variables. The parent injects:
//   - OMAC_BUILD_BROKER_REQUIRED=1 (always, even on setup failure)
//   - OMAC_CONTROL_BASE (the loopback control-plane URL)
//   - OMAC_BUILD_TOKEN (the per-parent crypto-random token)
//
// Managed build requires ALL THREE. Direct host execution is allowed
// only when the required marker AND all managed session variables are
// absent. A partial OMAC session env (any of OMAC_SOCKET, OMAC_BASE,
// OMAC_CONTROL_BASE, or OMAC_BUILD_TOKEN) blocks direct execution so a
// truncated/partial broker environment is never mistaken for build
// success.
const (
	envBuildBrokerRequired = "OMAC_BUILD_BROKER_REQUIRED"
	envControlBase         = "OMAC_CONTROL_BASE"
	envBuildToken          = "OMAC_BUILD_TOKEN"

	// Legacy/partial OMAC session env vars that, when present, block
	// direct execution even without the broker tuple.
	envOmacSocket = "OMAC_SOCKET"
	envOmacBase   = "OMAC_BASE"
)

// managedModeDecision reports whether the CLI should run a build via
// the managed broker, directly on the host, or fail closed. The
// decision is based on the process environment, not user input.
//
//   - managed: OMAC_BUILD_BROKER_REQUIRED=1 AND OMAC_CONTROL_BASE AND
//     OMAC_BUILD_TOKEN are all set. The CLI submits to the parent's
//     broker.
//   - direct: the required marker AND all managed session variables
//     are absent. The CLI runs the build engine in-process (the
//     existing host-terminal path).
//   - failClosed: the required marker is set but either broker value
//     is missing, OR a partial OMAC session env is present without the
//     complete broker tuple. The CLI exits 10 with a restart/upgrade
//     diagnostic.
type managedModeDecision int

const (
	managedModeDirect managedModeDecision = iota
	managedModeManaged
	managedModeFailClosed
)

// brokerEndpoint bundles the broker's loopback base URL and the
// per-parent bearer token. The two travel together through the managed
// path: decideManagedMode resolves them from the environment, and
// runBuildManaged / postCancel use them to reach the broker. A
// token-without-base (or base-without-token) is the fail-closed bug
// the decision detects.
type brokerEndpoint struct {
	Base  string
	Token string
}

// decideManagedMode inspects the environment and returns the mode plus
// the broker endpoint when managed.
func decideManagedMode() (managedModeDecision, brokerEndpoint) {
	required := os.Getenv(envBuildBrokerRequired) == "1"
	base := os.Getenv(envControlBase)
	token := os.Getenv(envBuildToken)
	// Any partial OMAC session env present?
	partial := os.Getenv(envOmacSocket) != "" ||
		os.Getenv(envOmacBase) != "" ||
		base != "" ||
		token != ""
	if required && base != "" && token != "" {
		return managedModeManaged, brokerEndpoint{Base: base, Token: token}
	}
	if required || partial {
		// Required marker set but tuple incomplete, OR a partial
		// OMAC session env present without the complete tuple: fail
		// closed. Direct host execution is forbidden in either case
		// so a truncated/partial broker environment is never mistaken
		// for build success.
		return managedModeFailClosed, brokerEndpoint{}
	}
	return managedModeDirect, brokerEndpoint{}
}

// runBuildManaged submits the build to the parent's broker over the
// loopback control plane and streams output to the CLI's stdout/stderr.
// It returns the CLI exit code.
//
// Signal handling: the first SIGINT/SIGTERM requests graceful
// cancellation (a POST to /cancel with stage=graceful); the second
// requests force (stage=force). The execute HTTP request is also
// canceled so the broker observes the disconnect and delivers graceful
// cancellation as a backstop.
//
// The broker frames the terminal result; the CLI translates the
// result class to the documented exit code. EOF before one valid
// result frame, a malformed or unknown frame, or a duplicate result is
// a service failure (exit 10) — a truncated stream is never treated as
// build success.
func runBuildManaged(args []string, env *Env, ep brokerEndpoint) int {
	// `omac build stop` reuses the execute operation but is refused in
	// this gate; the broker returns a 403 (pre-accepted) which the CLI
	// surfaces as a policy denial (exit 3) via the 403 branch below —
	// matching the existing direct-path behavior where stop is a
	// separate, broker-disabled path.
	body := buildbroker.ExecuteBody{
		Type:     "execute",
		Worktree: env.Workdir,
		Args:     args,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac build: encode request: %v\n", err)
		return buildrun.ExitServiceFailure
	}
	url := strings.TrimRight(ep.Base, "/") + buildbroker.ExecutePath
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(bodyBytes)))
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac build: build request: %v\n", err)
		return buildrun.ExitServiceFailure
	}
	req.Header.Set("Content-Type", buildbroker.ContentTypeJSON)
	req.Header.Set("Accept", buildbroker.AcceptNDJSON)
	req.Header.Set("Authorization", "Bearer "+ep.Token)

	// Cancellation: the first signal POSTs graceful; the second POSTs
	// force. The execute request's context is also canceled on the
	// first signal so the broker observes the disconnect as a backstop
	// (the broker's disconnect handler delivers graceful + forced
	// deadline independently of the cancel POST).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(ctx)

	// We need the request_id from the accepted frame to POST cancel.
	// Use a response-chained reader: read the NDJSON stream line by
	// line as it arrives, so we can issue the cancel POST the moment
	// we see the request_id.
	client := &http.Client{Timeout: 0} // no overall timeout; --max-duration bounds the build
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac build: broker unreachable: %v\n", err)
		return buildrun.ExitServiceFailure
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Pre-accepted error: surface as policy denial or service
		// failure based on the status code.
		msg := readBrokerError(resp)
		switch resp.StatusCode {
		case http.StatusServiceUnavailable:
			fmt.Fprintf(env.Stderr, "omac build: %s\n", msg)
			return buildrun.ExitServiceFailure
		case http.StatusForbidden, http.StatusUnauthorized:
			fmt.Fprintf(env.Stderr, "omac build: %s\n", msg)
			return ExitBuildPolicyDenied
		default:
			fmt.Fprintf(env.Stderr, "omac build: broker: %s\n", msg)
			return buildrun.ExitServiceFailure
		}
	}

	// Stream the NDJSON response. We read line by line; for output
	// frames we decode base64 and write raw bytes to the matching
	// stream; for the result frame we capture the class + exit code.
	// A separate goroutine handles signals and POSTs cancel.
	var (
		requestID    string
		resultClass  string
		resultExit   int
		gotResult    bool
		streamErr    error
		firstSignal  = make(chan os.Signal, 1)
		secondSignal = make(chan os.Signal, 1)
	)
	signal.Notify(firstSignal, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(firstSignal)
	// Second-signal escalation: listen for a second interrupt after
	// the first.
	go func() {
		<-firstSignal
		// First signal: graceful. POST cancel + cancel the execute
		// context as a backstop.
		if requestID != "" {
			postCancel(ep, requestID, "graceful")
		}
		cancel()
		// Now listen for a second signal.
		signal.Stop(firstSignal)
		signal.Notify(secondSignal, os.Interrupt, syscall.SIGTERM)
		<-secondSignal
		if requestID != "" {
			postCancel(ep, requestID, "force")
		}
	}()

	scanner := bufio.NewScanner(resp.Body)
	// Increase the scanner buffer so a large base64 output frame
	// (32 KiB raw -> ~44 KiB base64 + framing) fits in one line.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var f buildFrame
		if err := json.Unmarshal(line, &f); err != nil {
			// Malformed frame: service failure.
			fmt.Fprintf(env.Stderr, "omac build: malformed broker frame: %v\n", err)
			return buildrun.ExitServiceFailure
		}
		switch f.Type {
		case "accepted":
			requestID = f.RequestID
		case "output":
			data, derr := base64.StdEncoding.DecodeString(f.DataBase64)
			if derr != nil {
				fmt.Fprintf(env.Stderr, "omac build: malformed output frame: %v\n", derr)
				return buildrun.ExitServiceFailure
			}
			var w io.Writer
			switch f.Stream {
			case "stdout":
				w = env.Stdout
			case "stderr":
				w = env.Stderr
			default:
				fmt.Fprintf(env.Stderr, "omac build: unknown stream %q\n", f.Stream)
				return buildrun.ExitServiceFailure
			}
			if _, err := w.Write(data); err != nil {
				streamErr = err
			}
		case "result":
			if gotResult {
				// Duplicate result: service failure.
				fmt.Fprintln(env.Stderr, "omac build: duplicate result frame from broker")
				return buildrun.ExitServiceFailure
			}
			gotResult = true
			resultClass = f.Class
			resultExit = f.ExitCode
		default:
			fmt.Fprintf(env.Stderr, "omac build: unknown frame type %q\n", f.Type)
			return buildrun.ExitServiceFailure
		}
		if streamErr != nil {
			break
		}
	}
	if streamErr != nil && !gotResult {
		// A write failure (broken pipe) before the result: the broker
		// will observe the disconnect and cancel, but we cannot deliver
		// the result here. Treat as a service failure.
		fmt.Fprintf(env.Stderr, "omac build: output stream: %v\n", streamErr)
		return buildrun.ExitServiceFailure
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		// A read error after the result is fine (the broker closed
		// the connection); before the result it's a service failure.
		if !gotResult {
			fmt.Fprintf(env.Stderr, "omac build: broker stream: %v\n", err)
			return buildrun.ExitServiceFailure
		}
	}
	if !gotResult {
		// EOF before one valid result frame: service failure.
		fmt.Fprintln(env.Stderr, "omac build: broker closed stream without a result")
		return buildrun.ExitServiceFailure
	}
	// Translate the result class to the CLI exit code. The engine
	// assigned the class at the outcome site; the CLI never infers it
	// from the numeric code.
	return managedResultExitCode(resultClass, resultExit)
}

// managedResultExitCode translates the broker's result frame to the
// CLI exit code. The class is authoritative; the exit code is the
// documented mapping.
func managedResultExitCode(class string, exit int) int {
	switch buildengine.ResultClass(class) {
	case buildengine.ClassSuccess:
		return 0
	case buildengine.ClassBuildFailure:
		return exit
	case buildengine.ClassPolicyDenial:
		return ExitBuildPolicyDenied
	case buildengine.ClassCancelled:
		return ExitBuildCancelled
	case buildengine.ClassServiceFailure:
		return buildrun.ExitServiceFailure
	default:
		return buildrun.ExitServiceFailure
	}
}

// postCancel POSTs a cancel request to the broker. Best-effort: errors
// are swallowed because the cancel is a backstop (the disconnect
// handler on the broker side also delivers cancellation).
func postCancel(ep brokerEndpoint, requestID, stage string) {
	url := strings.TrimRight(ep.Base, "/") + buildbroker.CancelPathPrefix + requestID + buildbroker.CancelRouteSuffix
	body := fmt.Sprintf(`{"stage":%q}`, stage)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", buildbroker.ContentTypeJSON)
	req.Header.Set("Authorization", "Bearer "+ep.Token)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// readBrokerError reads a pre-accepted error response body as text.
func readBrokerError(resp *http.Response) string {
	body, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body))
}

// buildFrame is the CLI's view of a broker NDJSON frame. Only the
// fields the client needs are decoded; unknown fields are ignored
// (the broker validates the body shape; the client is permissive).
type buildFrame struct {
	Type       string `json:"type"`
	RequestID  string `json:"request_id"`
	Stream     string `json:"stream"`
	DataBase64 string `json:"data_base64"`
	Class      string `json:"class"`
	ExitCode   int    `json:"exit_code"`
	Message    string `json:"message,omitempty"`
}
