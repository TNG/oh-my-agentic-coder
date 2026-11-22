package mcpprobe

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

func TestMain(m *testing.M) {
	if os.Getenv("PROBE_FAKE_SERVER") == "1" {
		fakeServer()
		return
	}
	os.Exit(m.Run())
}

func fakeServer() {
	if path := os.Getenv("PROBE_DUMP"); path != "" {
		dump := fmt.Sprintf("pid=%d\ncwd=", os.Getpid())
		wd, _ := os.Getwd()
		dump += wd + "\n" + strings.Join(os.Environ(), "\n") + "\n"
		os.WriteFile(path, []byte(dump), 0o600)
	}
	if os.Getenv("PROBE_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "boom: missing credentials")
		os.Exit(3)
	}
	in := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &m) != nil || m.ID == nil {
			continue
		}
		reply := func(result any) {
			out.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		}
		switch m.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": "2025-06-18"})
		case "tools/list":
			switch m.Params.Cursor {
			case "":
				reply(map[string]any{"tools": []any{map[string]any{"name": "a", "annotations": map[string]any{"readOnlyHint": true}}}, "nextCursor": "p2"})
			case "p2":
				reply(map[string]any{"tools": []any{map[string]any{"name": "b"}}})
			}
		}
	}
}

func fakeEntry(t *testing.T, env map[string]string) *mcpreg.Entry {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e := map[string]string{"PROBE_FAKE_SERVER": "1"}
	for k, v := range env {
		e[k] = v
	}
	return &mcpreg.Entry{
		Def:         &mcpreg.Definition{Name: "fake", Server: mcpreg.ServerSpec{Command: []string{exe}, Env: e}},
		SnapshotDir: t.TempDir(),
	}
}

func TestListToolsFollowsPagination(t *testing.T) {
	tools, _, err := ListTools(context.Background(), fakeEntry(t, nil), nil, nil, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "a" || tools[1].Name != "b" {
		t.Fatalf("tools: %+v", tools)
	}
	if tools[0].ReadOnlyHint == nil || !*tools[0].ReadOnlyHint {
		t.Fatal("annotation not parsed")
	}
}

func TestChildEnvAndWorkingDirectory(t *testing.T) {
	dump := t.TempDir() + "/dump"
	e := fakeEntry(t, map[string]string{"PROBE_DUMP": dump})
	e.Def.Server.EnvPassthrough = []string{"PROBE_PASS", "PROBE_ABSENT"}
	e.Def.Secrets = []string{"TOKEN"}
	base := []string{"PATH=/usr/bin", "OMAC_SOCKET=/x", "OMAC_SIDECAR_SECRET=s", "SIDECAR_PORT=1", "PROBE_PASS=yes"}
	sec := map[string]secrets.Secret{"TOKEN": secrets.NewSecretString("tok")}
	if _, _, err := ListTools(context.Background(), e, sec, base, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "OMAC_") || strings.HasPrefix(line, "SIDECAR_") {
			t.Fatalf("child env holds %q", line)
		}
	}
	for _, want := range []string{"TOKEN=tok", "PROBE_PASS=yes", "PROBE_FAKE_SERVER=1", "PATH=/usr/bin", "cwd="} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %q in child env:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "PROBE_ABSENT") {
		t.Error("absent passthrough variable appeared")
	}
}

func TestChildIsGoneAfterProbe(t *testing.T) {
	dump := t.TempDir() + "/dump"
	if _, _, err := ListTools(context.Background(), fakeEntry(t, map[string]string{"PROBE_DUMP": dump}), nil, nil, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(dump)
	var pid int
	fmt.Sscanf(string(raw), "pid=%d", &pid)
	if pid == 0 {
		t.Fatal("no pid recorded")
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("child %d still exists: %v", pid, err)
	}
}

func TestStartFailureReturnsStderrTail(t *testing.T) {
	_, tail, err := ListTools(context.Background(), fakeEntry(t, map[string]string{"PROBE_FAIL": "1"}), nil, nil, 10*time.Second)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(tail, "boom: missing credentials") {
		t.Fatalf("tail: %q", tail)
	}
}

func TestMissingCommandFails(t *testing.T) {
	e := fakeEntry(t, nil)
	e.Def.Server.Command = []string{"/nonexistent/omac-probe-test"}
	if _, _, err := ListTools(context.Background(), e, nil, nil, time.Second); err == nil {
		t.Fatal("expected an error")
	}
}

func TestStderrTailIsBounded(t *testing.T) {
	tb := &tailBuffer{max: stderrTailBytes}
	tb.Write([]byte(strings.Repeat("x", 5000)))
	tb.Write([]byte("end"))
	if got := tb.String(); len(got) != stderrTailBytes || !strings.HasSuffix(got, "end") {
		t.Fatalf("tail len %d", len(got))
	}
}
