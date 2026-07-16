package intent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// popupLookupTimeout bounds the exact lookup on the network-popup path.
	// The call is loopback and the result is advisory (a miss just shows
	// "(not declared)"), so a wedged facade must not delay the popup: cap
	// it short rather than making the user wait.
	popupLookupTimeout = 500 * time.Millisecond
	// subtreeLookupTimeout bounds the subtree lookup on the learn-mode
	// teardown path, which is less latency-sensitive than a live popup.
	subtreeLookupTimeout = 2 * time.Second
)

// httpClient is shared across lookups so the loopback dial is amortized
// (connection reuse) instead of building a fresh transport per call. Per-call
// deadlines are applied via context, not a client-level Timeout.
var httpClient = &http.Client{}

// LookupOverHTTP queries a facade's GET /sandbox/intent?target=<target>
// endpoint for an exact agent-declared intent (used by the network
// popup, which looks up by host). Returns ("", false) on any error
// (network, missing endpoint, etc.) — the caller shows "(not declared)".
func LookupOverHTTP(baseURL, target string) (string, bool) {
	return lookupOverHTTP(baseURL, target, false, popupLookupTimeout)
}

// LookupSubtreeOverHTTP is like LookupOverHTTP but asks the facade for
// intents related to a directory subtree (target equal to, under, or an
// ancestor of the given path). Used by the folder learn-review, where
// the offered candidate is a reduced ancestor of the declared paths.
func LookupSubtreeOverHTTP(baseURL, target string) (string, bool) {
	return lookupOverHTTP(baseURL, target, true, subtreeLookupTimeout)
}

// MarkExplainMoreOverHTTP tells the facade (POST /sandbox/intent/explain)
// that the user clicked "Explain more" for target in the network popup. It
// is best-effort (fire-and-forget): errors are ignored because the flag is
// advisory. Called from the sandbox child's prompter, which cannot share the
// parent's registry memory. Bounded to popupLookupTimeout — it runs on the
// popup path, right after the user's click.
func MarkExplainMoreOverHTTP(baseURL, target string) {
	if baseURL == "" || target == "" {
		return
	}
	baseURL = strings.TrimRight(baseURL, "/")
	u := baseURL + "/sandbox/intent/explain?target=" + url.QueryEscape(target)
	ctx, cancel := context.WithTimeout(context.Background(), popupLookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

func lookupOverHTTP(baseURL, target string, subtree bool, timeout time.Duration) (string, bool) {
	if baseURL == "" || target == "" {
		return "", false
	}
	baseURL = strings.TrimRight(baseURL, "/")
	u := baseURL + "/sandbox/intent?target=" + url.QueryEscape(target)
	if subtree {
		u += "&subtree=1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", false
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var body struct {
		Target string `json:"target"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return "", false
	}
	if body.Reason == "" {
		return "", false
	}
	return body.Reason, true
}
