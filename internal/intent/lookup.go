package intent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LookupOverHTTP queries a facade's GET /sandbox/intent?target=<target>
// endpoint for an exact agent-declared intent (used by the network
// popup, which looks up by host). Returns ("", false) on any error
// (network, missing endpoint, etc.) — the caller shows "(not declared)".
func LookupOverHTTP(baseURL, target string) (string, bool) {
	return lookupOverHTTP(baseURL, target, false)
}

// LookupSubtreeOverHTTP is like LookupOverHTTP but asks the facade for
// intents related to a directory subtree (target equal to, under, or an
// ancestor of the given path). Used by the folder learn-review, where
// the offered candidate is a reduced ancestor of the declared paths.
func LookupSubtreeOverHTTP(baseURL, target string) (string, bool) {
	return lookupOverHTTP(baseURL, target, true)
}

func lookupOverHTTP(baseURL, target string, subtree bool) (string, bool) {
	if baseURL == "" || target == "" {
		return "", false
	}
	baseURL = strings.TrimRight(baseURL, "/")
	u := baseURL + "/sandbox/intent?target=" + url.QueryEscape(target)
	if subtree {
		u += "&subtree=1"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(u)
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
