package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
)

// rpc sends one request in the session and returns the raw reply message.
func rpc(t *testing.T, srv *httptest.Server, sid string, id int, method, params string) string {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)
	resp := do(t, "POST", srv.URL+"/mcp", body, withSession(sid))
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: status %d: %s", method, resp.StatusCode, b)
	}
	return readEvent(t, bufio.NewReader(resp.Body))
}

func callTool(t *testing.T, srv *httptest.Server, sid, name string) string {
	t.Helper()
	return rpc(t, srv, sid, 5, "tools/call", fmt.Sprintf(`{"name":%q}`, name))
}

func listedNames(t *testing.T, srv *httptest.Server, sid string) []string {
	t.Helper()
	var names []string
	cursor := ""
	for page := 0; page < 20; page++ {
		params := "{}"
		if cursor != "" {
			params = fmt.Sprintf(`{"cursor":%q}`, cursor)
		}
		var m struct {
			Result struct {
				Tools      []struct{ Name string } `json:"tools"`
				NextCursor string                  `json:"nextCursor"`
			} `json:"result"`
		}
		ev := rpc(t, srv, sid, 10+page, "tools/list", params)
		if err := json.Unmarshal([]byte(ev), &m); err != nil {
			t.Fatalf("tools/list reply %s: %v", ev, err)
		}
		for _, tl := range m.Result.Tools {
			names = append(names, tl.Name)
		}
		if cursor = m.Result.NextCursor; cursor == "" {
			return names
		}
	}
	t.Fatal("tools/list never ended")
	return nil
}

func toolsFile(o *Options) string { return o.Def.Server.Env["GATEWAY_FAKE_TOOLS"] }

func isPolicyError(ev, reason string) bool {
	return strings.Contains(ev, `"error"`) && strings.Contains(ev, `"`+reason+`"`)
}

func TestReviewedCallsAreAllowed(t *testing.T) {
	srv := newServer(t, nil)
	sid := initialize(t, srv)
	if ev := callTool(t, srv, sid, "echo"); !strings.Contains(ev, "called echo") {
		t.Fatalf("echo = %s", ev)
	}
	if ev := callTool(t, srv, sid, "wipe"); !strings.Contains(ev, "called wipe") {
		t.Fatalf("wipe = %s", ev)
	}
}

func TestDecideRefusesUnlistedAndOff(t *testing.T) {
	srv := newServer(t, nil)
	sid := initialize(t, srv)
	for _, name := range []string{"nonexistent", "hidden"} {
		if ev := callTool(t, srv, sid, name); !isPolicyError(ev, mcppolicy.ReasonUnlisted) {
			t.Errorf("%s: %s", name, ev)
		}
	}
	for _, method := range []string{"resources/read", "prompts/get"} {
		if ev := rpc(t, srv, sid, 6, method, `{}`); !isPolicyError(ev, mcppolicy.ReasonUnlisted) {
			t.Errorf("%s: %s", method, ev)
		}
	}
	resp := do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","method":"resources/read"}`, withSession(sid))
	if resp.StatusCode != 403 || resp.Header.Get("X-Omac-Reason") != mcppolicy.ReasonUnlisted {
		t.Errorf("notification: %d %q", resp.StatusCode, resp.Header.Get("X-Omac-Reason"))
	}
}

func TestAccessModeRefuses(t *testing.T) {
	srv := newServer(t, func(o *Options) { o.Policy.Access = mcppolicy.ReadOnly })
	sid := initialize(t, srv)
	if ev := callTool(t, srv, sid, "wipe"); !isPolicyError(ev, mcppolicy.ReasonAccess) {
		t.Fatalf("wipe = %s", ev)
	}
	if ev := callTool(t, srv, sid, "echo"); !strings.Contains(ev, "called echo") {
		t.Fatalf("echo = %s", ev)
	}
}

func TestMalformedRequestRefused(t *testing.T) {
	srv := newServer(t, nil)
	sid := initialize(t, srv)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","name":"wipe"}}`
	resp := do(t, "POST", srv.URL+"/mcp", body, withSession(sid))
	if resp.StatusCode != 400 || resp.Header.Get("X-Omac-Reason") != mcppolicy.ReasonUnparseable {
		t.Fatalf("duplicate key: %d %q", resp.StatusCode, resp.Header.Get("X-Omac-Reason"))
	}
	body = `{"jsonrpc":"2.0","id":"omac-gw-abc","method":"tools/list"}`
	if resp := do(t, "POST", srv.URL+"/mcp", body, withSession(sid)); resp.StatusCode != 400 {
		t.Fatalf("reserved id: %d", resp.StatusCode)
	}
}

func TestChangedToolsBlockCalls(t *testing.T) {
	var file string
	srv := newServer(t, func(o *Options) { file = toolsFile(o) })
	changed := append([]fakeTool{}, baseTools...)
	changed[0].Description = "echoes, and now exfiltrates"
	changed = append(changed[:1], changed[2:]...) // wipe removed
	changed = append(changed, fakeTool{"sneaky", "new"})
	writeTools(t, file, changed...)

	sid := initialize(t, srv)
	ev := callTool(t, srv, sid, "echo")
	if !isPolicyError(ev, mcppolicy.ReasonChanged) {
		t.Fatalf("echo = %s", ev)
	}
	for _, want := range []string{`added: \"sneaky\"`, `removed: \"wipe\"`, `changed: \"echo\"`, "omac mcp review fake"} {
		if !strings.Contains(ev, want) {
			t.Errorf("error lacks %q: %s", want, ev)
		}
	}
	if ev := callTool(t, srv, sid, "debug"); !isPolicyError(ev, mcppolicy.ReasonChanged) {
		t.Errorf("another tool still callable: %s", ev)
	}
	if got := listedNames(t, srv, sid); strings.Contains(strings.Join(got, ","), "echo") || strings.Contains(strings.Join(got, ","), "sneaky") {
		t.Errorf("listed %v", got)
	}
	var st struct{ Review string }
	getStatus(t, srv, &st)
	if st.Review != "changed" {
		t.Errorf("status review = %q", st.Review)
	}
}

func TestUnreviewedBlocksCallsAndListsNothing(t *testing.T) {
	srv := newServer(t, func(o *Options) { o.Policy.Review = nil })
	sid := initialize(t, srv)
	ev := callTool(t, srv, sid, "echo")
	if !isPolicyError(ev, mcppolicy.ReasonUnreviewed) || !strings.Contains(ev, "omac mcp tools fake --classify") {
		t.Fatalf("echo = %s", ev)
	}
	if got := listedNames(t, srv, sid); len(got) != 0 {
		t.Errorf("listed %v", got)
	}
	var st struct{ Review string }
	getStatus(t, srv, &st)
	if st.Review != "not-reviewed" {
		t.Errorf("status review = %q", st.Review)
	}
}

func TestListChangedTriggersRecheck(t *testing.T) {
	var file string
	srv := newServer(t, func(o *Options) { file = toolsFile(o) })
	sid := initialize(t, srv)
	if ev := callTool(t, srv, sid, "echo"); !strings.Contains(ev, "called echo") {
		t.Fatalf("echo = %s", ev)
	}
	changed := append([]fakeTool{}, baseTools...)
	changed[0].Description = "different"
	writeTools(t, file, changed...)
	// without a notification the verdict is cached
	if ev := callTool(t, srv, sid, "echo"); !strings.Contains(ev, "called echo") {
		t.Fatalf("cached verdict expected, got %s", ev)
	}
	callTool(t, srv, sid, "relist")
	if ev := callTool(t, srv, sid, "echo"); !isPolicyError(ev, mcppolicy.ReasonChanged) {
		t.Fatalf("after list_changed: %s", ev)
	}
	writeTools(t, file, baseTools...)
	callTool(t, srv, sid, "relist") // refused, but must not matter
	do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, withSession(sid))
	deadline := time.Now().Add(3 * time.Second)
	for {
		if ev := callTool(t, srv, sid, "echo"); strings.Contains(ev, "called echo") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("review check did not recover after the tools matched again")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestToolsListFilteredOnEveryPage(t *testing.T) {
	srv := newServer(t, func(o *Options) {
		o.Def.Server.Env["GATEWAY_FAKE_PAGE"] = "1"
		o.Policy.Access = mcppolicy.ReadWrite
	})
	sid := initialize(t, srv)
	got := strings.Join(listedNames(t, srv, sid), ",")
	// wipe is a delete tool under read-write access, hidden is off
	if got != "echo,debug,relist" {
		t.Fatalf("listed %q", got)
	}
}

func TestChangedToolNeverListed(t *testing.T) {
	var file string
	srv := newServer(t, func(o *Options) { file = toolsFile(o) })
	changed := append([]fakeTool{}, baseTools...)
	changed[0].Description = "tampered"
	changed = append(changed, fakeTool{"unclassified", "x"})
	writeTools(t, file, changed...)
	got := listedNames(t, srv, initialize(t, srv))
	if strings.Join(got, ",") != "wipe,debug,relist" {
		t.Fatalf("listed %v", got)
	}
}

func TestInternalIDsNeverReachClient(t *testing.T) {
	srv := newServer(t, nil)
	sid := initialize(t, srv)
	stream := do(t, "GET", srv.URL+"/mcp", "", withSession(sid))
	r := bufio.NewReader(stream.Body)
	resp := do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","id":"c1","method":"tools/call","params":{"name":"echo"}}`, withSession(sid))
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "omac-gw-") || !strings.Contains(string(b), `"id":"c1"`) {
		t.Fatalf("reply = %s", b)
	}
	do(t, "POST", srv.URL+"/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, withSession(sid))
	if ev := readEvent(t, r); !strings.Contains(ev, "list_changed") || strings.Contains(ev, "omac-gw-") {
		t.Fatalf("stream event = %s", ev)
	}
	stream.Body.Close()
}

func TestUnknownSessionHeader(t *testing.T) {
	srv := newServer(t, nil)
	for _, method := range []string{"POST", "GET", "DELETE"} {
		resp := do(t, method, srv.URL+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, withSession("gone"))
		if resp.StatusCode != 404 || resp.Header.Get("X-Omac-Reason") != mcppolicy.ReasonUnknownSession {
			t.Errorf("%s: %d %q", method, resp.StatusCode, resp.Header.Get("X-Omac-Reason"))
		}
	}
}

func getStatus(t *testing.T, srv *httptest.Server, into any) {
	t.Helper()
	resp := do(t, "GET", srv.URL+"/status", "", nil)
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "description") || strings.Contains(string(b), "echoes") {
		t.Errorf("status leaks tool text: %s", b)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("status %s: %v", b, err)
	}
}

func TestStatusPinned(t *testing.T) {
	srv := newServer(t, nil)
	initialize(t, srv)
	var st struct {
		Status string
		Review string
	}
	getStatus(t, srv, &st)
	if st.Status != "ok" || st.Review != "reviewed" {
		t.Fatalf("status = %+v", st)
	}
}

func TestNewSnapshotDirDoesNotAffectOpenGateway(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "snapshot-1")
	if err := os.Mkdir(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	srv := newServer(t, func(o *Options) { o.Dir = oldDir })
	first := initialize(t, srv)
	// a new review writes a newer snapshot next to the old one; the old one stays
	if err := os.Mkdir(filepath.Join(root, "snapshot-2"), 0o700); err != nil {
		t.Fatal(err)
	}
	second := initialize(t, srv)
	for _, sid := range []string{first, second} {
		if got, want := realPath(t, debugInfo(t, srv, sid).CWD), realPath(t, oldDir); got != want {
			t.Errorf("cwd = %q, want %q", got, want)
		}
		if ev := callTool(t, srv, sid, "echo"); !strings.Contains(ev, "called echo") {
			t.Errorf("echo = %s", ev)
		}
	}
}

func TestNilPolicyRejected(t *testing.T) {
	o := testOptions(t)
	o.Policy = nil
	if _, _, err := Handler(o); err == nil {
		t.Fatal("gateway without a policy was accepted")
	}
}

func TestChildDiesWithGateway(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Pdeathsig exists only on Linux")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	exe, _ := os.Executable()
	parent := exec.Command(exe, "-test.run=^TestPdeathsigParent$", "-test.v")
	parent.Env = append(os.Environ(), "GATEWAY_PDEATHSIG_PIDFILE="+pidFile)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer parent.Process.Kill()
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for pid == 0 {
		if b, err := os.ReadFile(pidFile); err == nil {
			fmt.Sscan(string(b), &pid)
		}
		if time.Now().After(deadline) {
			t.Fatal("parent did not report a child pid")
		}
		time.Sleep(20 * time.Millisecond)
	}
	parent.Process.Kill()
	parent.Wait()
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("child %d outlived its killed gateway", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPdeathsigParent runs only as the helper of TestChildDiesWithGateway: it
// starts a gateway session and reports the child's pid.
func TestPdeathsigParent(t *testing.T) {
	pidFile := os.Getenv("GATEWAY_PDEATHSIG_PIDFILE")
	if pidFile == "" {
		t.Skip("helper process only")
	}
	srv := newServer(t, nil)
	sid := initialize(t, srv)
	pid := debugInfo(t, srv, sid).PID
	os.WriteFile(pidFile, []byte(fmt.Sprint(pid)), 0o600)
	time.Sleep(time.Minute)
}
