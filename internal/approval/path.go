package approval

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CleanPath normalizes the request path relative to the mount, once, for both
// matching and forwarding. rawPath is the escaped path as received
// (url.URL.EscapedPath of the part after the mount). It returns:
//
//   - segs, the decoded segments for Set.Lookup; a trailing slash is kept as a
//     final "" segment so "/status/" is not "/status";
//   - forward, the escaped path to send upstream, rebuilt from segs so the
//     sidecar sees exactly what was matched.
//
// "." and ".." segments are resolved (never above the root) and empty middle
// segments ("//") collapse. A path that cannot be normalized unambiguously
// is an error and the caller rejects the request: an encoded slash (%2F), a
// backslash (raw or %5C), a NUL or control character, or invalid
// percent-encoding. A second layer of encoding (%252e) stays literal text.
func CleanPath(rawPath string) (segs []string, forward string, err error) {
	if rawPath == "" {
		rawPath = "/"
	}
	if !strings.HasPrefix(rawPath, "/") {
		rawPath = "/" + rawPath
	}
	parts := strings.Split(rawPath[1:], "/")
	trailing := len(parts) > 1 && parts[len(parts)-1] == "" || rawPath == "/"
	var out []string
	for _, part := range parts {
		dec, derr := url.PathUnescape(part)
		if derr != nil {
			return nil, "", fmt.Errorf("invalid percent-encoding in path")
		}
		if strings.ContainsAny(dec, "/\\") {
			return nil, "", fmt.Errorf("encoded slash or backslash in path")
		}
		for _, r := range dec {
			if r < 0x20 || r == 0x7f {
				return nil, "", fmt.Errorf("control character in path")
			}
		}
		switch dec {
		case "", ".":
			// "//" and "/./" collapse.
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, dec)
		}
	}
	// A trailing "." or ".." also ends in a directory: "/a/.." is "/".
	last := parts[len(parts)-1]
	if last == "." || last == ".." {
		trailing = true
	}
	if trailing && len(out) > 0 {
		out = append(out, "")
	}
	enc := make([]string, len(out))
	for i, s := range out {
		enc[i] = (&url.URL{Path: s}).EscapedPath()
	}
	return out, "/" + strings.Join(enc, "/"), nil
}

// overrideHeaders carry a method the server may honor instead of the one on
// the request line, which would sidestep a method-based rule.
var overrideHeaders = []string{
	"X-HTTP-Method-Override",
	"X-Method-Override",
	"X-HTTP-Method",
}

// StripOverrideHeaders removes method-override headers from h and returns the
// names it removed, for the audit trail.
func StripOverrideHeaders(h http.Header) []string {
	var removed []string
	for _, name := range overrideHeaders {
		if h.Get(name) != "" {
			removed = append(removed, name)
		}
		h.Del(name)
	}
	return removed
}
