// Package mcpprobe starts a registered MCP server on the host and lists its tools.
package mcpprobe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpproc"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
	"github.com/TNG/oh-my-agentic-coder/internal/sidecarauth"
)

const (
	stderrTailBytes = 2 << 10
	maxLineBytes    = 16 << 20
	killWait        = 2 * time.Second
	protocolVersion = "2025-06-18"
)

// ListTools starts the server of e the way the gateway does and returns its
// complete tool list and the last 2 KiB of its stderr, also on failure. The
// child gets baseEnv without OMAC_* and SIDECAR_* variables, the definition's
// env, mode and passthrough variables found in baseEnv, and sec. It and its
// process group are killed before ListTools returns.
func ListTools(ctx context.Context, e *mcpreg.Entry, sec map[string]secrets.Secret, baseEnv []string, timeout time.Duration) ([]mcppolicy.ToolInfo, string, error) {
	argv := e.Def.Server.Command
	if len(argv) == 0 {
		return nil, "", errors.New("definition has no server command")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tail := &tailBuffer{max: stderrTailBytes}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = e.SnapshotDir
	cmd.Env = childEnv(e.Def, sec, baseEnv)
	cmd.Stderr = tail
	cmd.WaitDelay = killWait
	mcpproc.SetProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	if err := cmd.Start(); err != nil {
		return nil, tail.String(), fmt.Errorf("start %s: %w", argv[0], err)
	}
	tools, err := newClient(ctx, stdin, stdout).handshakeAndList(ctx)
	stdin.Close()
	mcpproc.Kill(cmd)
	cmd.Wait()
	if err != nil {
		return nil, tail.String(), err
	}
	return tools, tail.String(), nil
}

func childEnv(def *mcpreg.Definition, sec map[string]secrets.Secret, baseEnv []string) []string {
	vars := map[string]string{}
	var order []string
	set := func(name, value string) {
		if sidecarauth.Reserved(name) {
			return
		}
		if _, dup := vars[name]; !dup {
			order = append(order, name)
		}
		vars[name] = value
	}
	base := map[string]string{}
	for _, kv := range baseEnv {
		i := strings.IndexByte(kv, '=')
		if i <= 0 || strings.HasPrefix(kv, "OMAC_") || strings.HasPrefix(kv, "SIDECAR_") {
			continue
		}
		base[kv[:i]] = kv[i+1:]
		set(kv[:i], kv[i+1:])
	}
	for _, name := range def.Server.EnvPassthrough {
		if v, ok := base[name]; ok {
			set(name, v)
		}
	}
	for name, value := range def.Server.Env {
		set(name, value)
	}
	if m := def.Mode; m != nil {
		set(m.Name, m.Value)
	}
	for name, value := range sec {
		set(name, value.ExposeString())
	}
	out := make([]string, 0, len(order))
	for _, name := range order {
		out = append(out, name+"="+vars[name])
	}
	return out
}

type message struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type client struct {
	stdin io.Writer
	msgs  chan message
	next  int
}

func newClient(ctx context.Context, stdin io.Writer, stdout io.Reader) *client {
	c := &client{stdin: stdin, msgs: make(chan message, 16)}
	go func() {
		defer close(c.msgs)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), maxLineBytes)
		for sc.Scan() {
			var m message
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				select {
				case c.msgs <- m:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return c
}

func (c *client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

// Call implements mcppolicy.RoundTripper.
func (c *client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.next++
	id := c.next
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("%s: write: %w", method, err)
	}
	want := fmt.Sprint(id)
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s: %w", method, ctx.Err())
		case m, ok := <-c.msgs:
			if !ok {
				return nil, fmt.Errorf("%s: server exited before replying", method)
			}
			if string(m.ID) != want {
				continue
			}
			if m.Error != nil {
				return nil, fmt.Errorf("%s: server error %d: %s", method, m.Error.Code, m.Error.Message)
			}
			return m.Result, nil
		}
	}
}

func (c *client) handshakeAndList(ctx context.Context) ([]mcppolicy.ToolInfo, error) {
	_, err := c.Call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "omac-probe", "version": "1"},
	})
	if err != nil {
		return nil, err
	}
	if err := c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		return nil, fmt.Errorf("notifications/initialized: %w", err)
	}
	return mcppolicy.ListTools(ctx, c)
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
