package cli

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/gitservice"
)

func runGitClientTest(t *testing.T, args []string, post func(http.ResponseWriter, *http.Request, gitservice.Request)) (int, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "omac-git-client-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/git/status" {
			_ = json.NewEncoder(w).Encode(gitservice.Result{State: &gitservice.State{Repository: dir, Session: "session", Head: strings.Repeat("a", 40), Branch: "main", Index: "index", Branches: map[string]string{"main": strings.Repeat("a", 40)}}})
			return
		}
		var req gitservice.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		post(w, r, req)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	t.Setenv("OMAC_SOCKET", socket)
	t.Setenv("OMAC_GIT_BASE", "http://127.0.0.1:1/git")
	t.Setenv("OMAC_FACADE_TOKEN", "test-token")
	env := devnullEnv(t)
	env.Workdir = dir
	output, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { output.Close() })
	env.Stderr = output
	code := runGit(args, env)
	data, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, string(data)
}

func TestGitClientFailureHints(t *testing.T) {
	for _, tc := range []struct {
		name         string
		code         int
		body, reason string
		unknown      bool
	}{
		{"denied", 403, "omac: approval-denied\n", "approval-denied", false},
		{"approval unavailable", 503, "omac: approval-unavailable\n", "approval-unavailable", false},
		{"busy", 503, `{"error":"repository busy; retry status, not a new mutation"}`, "", false},
		{"stale", 409, `{"error":"repository changed since the request; inspect status"}`, "", false},
		{"partial", 409, `{"error":"partial mutation","outcome_unknown":true}`, "", true},
		{"malformed", 409, `{`, "", true},
		{"empty success", 200, `{}`, "", true},
		{"truncated", 200, `{`, "truncated", true},
		{"lost response", 0, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, output := runGitClientTest(t, []string{"stage", "--", "source"}, func(w http.ResponseWriter, r *http.Request, req gitservice.Request) {
				if tc.code == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				w.Header().Set("X-Omac-Reason", tc.reason)
				if tc.reason == "truncated" {
					w.Header().Set("Content-Length", "100")
				}
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			})
			if code == ExitOK || strings.Contains(output, "Do not retry with a new ID") != tc.unknown || strings.Contains(output, "Request rejected") == tc.unknown {
				t.Fatalf("exit=%d output=%s", code, output)
			}
		})
	}
}

func TestGitClientChecksMutationReceipt(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		code, output := runGitClientTest(t, []string{"stage", "--", "source"}, func(w http.ResponseWriter, r *http.Request, req gitservice.Request) {
			id := req.ID
			if wrong {
				id = "wrong-session:nonce"
			}
			_ = json.NewEncoder(w).Encode(gitservice.Result{ID: id, State: &gitservice.State{Index: "updated"}})
		})
		if (code == ExitOK) == wrong || strings.Contains(output, "Do not retry") != wrong {
			t.Fatalf("wrong=%t exit=%d: %s", wrong, code, output)
		}
	}
}

func TestGitClientReportsUnknownBranchBeforePosting(t *testing.T) {
	code, output := runGitClientTest(t, []string{"switch", "missing"}, func(w http.ResponseWriter, r *http.Request, req gitservice.Request) {
		t.Error("posted nonexistent branch")
		w.WriteHeader(400)
	})
	if code != ExitMisuse || !strings.Contains(output, `no local branch named "missing"`) || strings.Contains(output, "Do not retry") {
		t.Fatalf("exit=%d: %s", code, output)
	}
}
