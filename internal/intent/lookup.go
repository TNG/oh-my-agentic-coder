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
// endpoint for an agent-declared intent. Returns the reason and whether
// one was found. Returns ("", false) on any error (network, missing
// endpoint, etc.) — the popup shows "(not declared)".
func LookupOverHTTP(baseURL, target string) (string, bool) {
	if baseURL == "" || target == "" {
		return "", false
	}
	baseURL = strings.TrimRight(baseURL, "/")
	u := baseURL + "/sandbox/intent?target=" + url.QueryEscape(target)
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
