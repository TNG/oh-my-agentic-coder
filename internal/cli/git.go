package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/gitservice"
)

func runGit(args []string, env *Env) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(env.Stdout, "Usage: omac git status | stage -- <files> | commit -m <message> [--signoff] | branch <name> | switch <branch> | operation <id>")
		return ExitOK
	}
	if args[0] == "--compat" {
		args = args[1:]
		if len(args) == 0 {
			return ExitMisuse
		}
		switch args[0] {
		case "status", "log", "diff", "show", "rev-parse", "ls-files", "ls-tree":
			cmd := exec.Command("/usr/bin/git", args...)
			cmd.Dir = env.Workdir
			cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = env.Stdin, env.Stdout, env.Stderr
			if err := cmd.Run(); err != nil {
				if e, ok := err.(*exec.ExitError); ok {
					return e.ExitCode()
				}
				fmt.Fprintln(env.Stderr, err)
				return ExitIOError
			}
			return ExitOK
		case "add":
			args = append([]string{"stage"}, args[1:]...)
		}
	}
	action, req, err := parseGitRequest(args, env.Stderr)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac git:", err)
		return ExitMisuse
	}
	base := os.Getenv("OMAC_GIT_BASE")
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSuffix(u.Path, "/") != "/git" {
		fmt.Fprintln(env.Stderr, "omac git: start with --git-service=prototype; OMAC_GIT_BASE must name the local Git facade")
		return ExitConfigInvalid
	}
	transport := &http.Transport{Proxy: nil}
	if socket := os.Getenv("OMAC_SOCKET"); socket != "" {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}
	}
	client := &http.Client{Timeout: 2 * time.Minute, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	call := func(method, path string, body any) (gitservice.Result, error) {
		var input io.Reader
		if body != nil {
			b, e := json.Marshal(body)
			if e != nil {
				return gitservice.Result{}, e
			}
			input = bytes.NewReader(b)
		}
		r, e := http.NewRequest(method, strings.TrimSuffix(base, "/")+"/"+path, input)
		if e != nil {
			return gitservice.Result{}, e
		}
		r.Header.Set("X-Omac-Facade-Token", os.Getenv("OMAC_FACADE_TOKEN"))
		r.Header.Set("Content-Type", "application/json")
		uncertain := gitservice.Result{Unknown: method == http.MethodPost}
		resp, e := client.Do(r)
		if e != nil {
			return uncertain, e
		}
		defer resp.Body.Close()
		reason := resp.Header.Get("X-Omac-Reason")
		if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusServiceUnavailable) &&
			(reason == "approval-denied" || reason == "approval-timeout" || reason == "approval-abandoned" || reason == "approval-unavailable") {
			return gitservice.Result{}, fmt.Errorf("HTTP %d: omac: %s", resp.StatusCode, reason)
		}
		const maxResponse = 4 << 20
		data, e := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
		if e != nil {
			return uncertain, e
		}
		var result gitservice.Result
		if len(data) > maxResponse || json.Unmarshal(data, &result) != nil {
			return uncertain, fmt.Errorf("HTTP %d: invalid or oversized Git response", resp.StatusCode)
		}
		if method == http.MethodPost && result.ID != "" && result.ID != req.ID {
			return uncertain, fmt.Errorf("Git response has a different operation ID")
		}
		if resp.StatusCode != http.StatusOK {
			if result.Error == "" {
				return uncertain, fmt.Errorf("HTTP %d: missing Git error details", resp.StatusCode)
			}
			return result, fmt.Errorf("HTTP %d: %s", resp.StatusCode, result.Error)
		}
		if method == http.MethodPost && (result.ID != req.ID || result.State == nil || result.Unknown || result.Error != "") {
			return uncertain, fmt.Errorf("incomplete Git mutation result")
		}
		return result, nil
	}
	readPath := "status"
	if action == "operation" {
		readPath = "operations/" + url.PathEscape(args[1])
	}
	result, err := call("GET", readPath, nil)
	if err == nil && action != "status" && action != "operation" {
		if result.State == nil {
			err = fmt.Errorf("service returned no repository state")
		} else {
			if _, err := gitClientDirectory(result.State.Repository, env.Workdir); err != nil {
				fmt.Fprintln(env.Stderr, "omac git:", err)
				return ExitMisuse
			}
			if action == "stage" {
				req.Paths, err = gitStagePaths(result.State.Repository, env.Workdir, req.Paths)
				if err != nil {
					fmt.Fprintln(env.Stderr, "omac git:", err)
					return ExitMisuse
				}
			}
			req.Expected = result.State
			if action == "switch" {
				var exists bool
				req.Target, exists = result.State.Branches[req.Branch]
				if !exists {
					fmt.Fprintf(env.Stderr, "omac git: no local branch named %q\n", req.Branch)
					return ExitMisuse
				}
			}
			var nonce [16]byte
			if _, err = rand.Read(nonce[:]); err == nil {
				req.ID = result.State.Session + ":" + hex.EncodeToString(nonce[:])
				if action == "branches" {
					req.Target = result.State.Head
				}
				result, err = call("POST", action, req)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac git:", err)
		if req.ID != "" {
			if result.Unknown {
				fmt.Fprintf(env.Stderr, "Operation %s. Do not retry with a new ID; inspect omac git operation %s and omac git status.\n", req.ID, req.ID)
			} else {
				fmt.Fprintln(env.Stderr, "Request rejected; inspect omac git status and re-run when ready.")
			}
		}
		return ExitGeneric
	}
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return ExitIOError
	}
	return ExitOK
}

func gitClientDirectory(root, cwd string) (string, error) {
	if root == "" || !filepath.IsAbs(root) || !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("service returned no repository directory")
	}
	root, cwd = filepath.Clean(root), filepath.Clean(cwd)
	prefix, err := filepath.Rel(root, cwd)
	if err != nil || prefix == ".." || strings.HasPrefix(prefix, "../") {
		return "", fmt.Errorf("Git service operations must run inside the enrolled checkout")
	}
	for dir := cwd; dir != root; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
			return "", fmt.Errorf("Git service operations cannot run from a nested repository")
		}
	}
	return prefix, nil
}

func gitStagePaths(root, cwd string, paths []string) ([]string, error) {
	prefix, err := gitClientDirectory(root, cwd)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(paths))
	for i, path := range paths {
		if filepath.IsAbs(path) {
			return nil, fmt.Errorf("stage requires relative paths")
		}
		out[i] = filepath.Join(prefix, path)
	}
	return out, nil
}

func parseGitRequest(args []string, w io.Writer) (string, gitservice.Request, error) {
	var req gitservice.Request
	if len(args) == 0 {
		return "", req, fmt.Errorf("missing operation")
	}
	switch args[0] {
	case "status":
		if len(args) == 1 {
			return "status", req, nil
		}
	case "operation":
		if len(args) == 2 && !strings.ContainsAny(args[1], "/\\?#\x00") {
			return "operation", req, nil
		}
	case "stage":
		paths := args[1:]
		literal := len(paths) > 0 && paths[0] == "--"
		if literal {
			paths = paths[1:]
		}
		if len(paths) > 0 {
			for _, p := range paths {
				if !literal && strings.HasPrefix(p, "-") {
					return "", req, fmt.Errorf("stage requires explicit paths; no Git options")
				}
			}
			req.Paths = paths
			return "stage", req, nil
		}
	case "commit":
		fs := flag.NewFlagSet("commit", flag.ContinueOnError)
		fs.SetOutput(w)
		fs.StringVar(&req.Message, "m", "", "commit message")
		fs.BoolVar(&req.Signoff, "signoff", false, "add DCO sign-off")
		fs.BoolVar(&req.Signoff, "s", false, "add DCO sign-off")
		if err := fs.Parse(args[1:]); err != nil {
			return "", req, err
		}
		if req.Message != "" && fs.NArg() == 0 {
			return "commit", req, nil
		}
	case "branch", "switch":
		if len(args) == 2 && !strings.HasPrefix(args[1], "-") {
			req.Branch = args[1]
			if args[0] == "branch" {
				return "branches", req, nil
			}
			return "switch", req, nil
		}
	}
	return "", req, fmt.Errorf("unsupported Git syntax; use omac git --help (no network, config, force or interactive operations)")
}
