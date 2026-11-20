// Command approvals-sidecar is the sidecar of the approvals E2E tests. It
// answers every request with 200 and appends what reached it, one JSON line
// per request, to the file named by its only argument, so a test can tell
// which requests the facade forwarded and in which form. A request under
// /live/ gets a 101 Switching Protocols and the connection is closed after
// it. It does not require an Upgrade header: the facade does not forward
// one today.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

type record struct {
	Method   string            `json:"method"`
	Path     string            `json:"path"` // escaped, as on the request line
	Query    string            `json:"query"`
	Upgrade  string            `json:"upgrade,omitempty"`
	Override map[string]string `json:"override,omitempty"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: approvals-sidecar <record-file>")
		os.Exit(2)
	}
	out, err := os.OpenFile(os.Args[1], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var mu sync.Mutex
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := record{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Upgrade: r.Header.Get("Upgrade")}
		for name, v := range r.Header {
			if strings.Contains(strings.ToLower(strings.ReplaceAll(name, "_", "-")), "method") {
				if rec.Override == nil {
					rec.Override = map[string]string{}
				}
				rec.Override[name] = strings.Join(v, ",")
			}
		}
		if r.URL.Path != "/status" {
			line, _ := json.Marshal(rec)
			mu.Lock()
			out.Write(append(line, '\n'))
			mu.Unlock()
		}
		if strings.HasPrefix(r.URL.Path, "/live/") {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			buf.Flush()
			conn.Close()
			return
		}
		fmt.Fprintf(w, "ok %s %s\n", r.Method, r.URL.EscapedPath())
	})
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("SIDECAR_PORT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	http.Serve(ln, handler)
}
