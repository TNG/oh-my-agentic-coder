package approver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
)

type fakeChooser struct {
	mu      sync.Mutex
	answers []string // consumed in order; the last repeats
	texts   []string
	active  int32
	maxAct  int32
	block   chan struct{} // when set, Choose waits for it or ctx
	delay   time.Duration
}

func (c *fakeChooser) Notify(string, string) {}
func (c *fakeChooser) Choose(ctx context.Context, _, text string, _ []string, _ string) (string, error) {
	n := atomic.AddInt32(&c.active, 1)
	for {
		m := atomic.LoadInt32(&c.maxAct)
		if n <= m || atomic.CompareAndSwapInt32(&c.maxAct, m, n) {
			break
		}
	}
	defer atomic.AddInt32(&c.active, -1)
	c.mu.Lock()
	c.texts = append(c.texts, text)
	ans := c.answers[0]
	if len(c.answers) > 1 {
		c.answers = c.answers[1:]
	}
	c.mu.Unlock()
	if c.block != nil {
		select {
		case <-c.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	time.Sleep(c.delay)
	return ans, nil
}
func (c *fakeChooser) asked() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.texts) }

func req(path string) facade.ApprovalRequest {
	return facade.ApprovalRequest{Skill: "tng-slack", Mount: "tng-slack", Method: "POST", Path: path,
		Match: "POST " + path, Description: "Posts a message"}
}

// oneProject binds every request to one project.
func oneProject(string) Scope { return Scope{Key: "wd-test", Dir: "/p/test"} }

func newApprover(t *testing.T, c *fakeChooser) *Approver {
	t.Helper()
	return New(Config{ScopeFor: oneProject, Chooser: c, Store: NewStore(t.TempDir()), Timeout: 2 * time.Second})
}

func TestOnceIsNotRemembered(t *testing.T) {
	c := &fakeChooser{answers: []string{LabelOnce}}
	a := newApprover(t, c)
	for i := 0; i < 2; i++ {
		d := a.Approve(context.Background(), req("/send"))
		if !d.Allow || d.Scope != "once" || d.Source != "prompt" {
			t.Fatalf("decision = %+v", d)
		}
	}
	if c.asked() != 2 {
		t.Errorf("asked %d times, want 2", c.asked())
	}
}

func TestSessionIsRememberedPerEndpoint(t *testing.T) {
	c := &fakeChooser{answers: []string{LabelSession}}
	a := newApprover(t, c)
	a.Approve(context.Background(), req("/send"))
	d := a.Approve(context.Background(), req("/send"))
	if !d.Allow || d.Source != "session" || c.asked() != 1 {
		t.Errorf("decision = %+v asked=%d", d, c.asked())
	}
	a.Approve(context.Background(), req("/other"))
	if c.asked() != 2 {
		t.Error("a session verdict for one endpoint admitted another")
	}
}

func TestAlwaysPersistsAcrossRestartAndRevokes(t *testing.T) {
	dir := t.TempDir()
	c := &fakeChooser{answers: []string{LabelAlways}}
	a := New(Config{ScopeFor: oneProject, Chooser: c, Store: NewStore(dir), Timeout: time.Second})
	if d := a.Approve(context.Background(), req("/send")); !d.Allow || d.Scope != "always" {
		t.Fatalf("decision = %+v", d)
	}
	c2 := &fakeChooser{answers: []string{LabelDeny}}
	a2 := New(Config{ScopeFor: oneProject, Chooser: c2, Store: NewStore(dir), Timeout: time.Second})
	d := a2.Approve(context.Background(), req("/send"))
	if !d.Allow || d.Source != "always" || c2.asked() != 0 {
		t.Errorf("after restart: %+v asked=%d", d, c2.asked())
	}
	vs, err := NewStore(dir).List()
	if err != nil || len(vs) != 1 || vs[0].Skill != "tng-slack" || vs[0].Match != "POST /send" {
		t.Fatalf("list = %+v %v", vs, err)
	}
	if ok, _ := NewStore(dir).Revoke(vs[0].ID); !ok {
		t.Fatal("revoke failed")
	}
	if d := a2.Approve(context.Background(), req("/send")); d.Allow {
		t.Errorf("revoked verdict still allowed: %+v", d)
	}
}

func TestAlwaysForAnUnlistedRouteDoesNotCoverADeclaredEntry(t *testing.T) {
	dir := t.TempDir()
	unlisted := req("/send")
	unlisted.Unlisted, unlisted.Description = true, ""
	a := New(Config{ScopeFor: oneProject, Chooser: &fakeChooser{answers: []string{LabelAlways}}, Store: NewStore(dir), Timeout: time.Second})
	if d := a.Approve(context.Background(), unlisted); !d.Allow || d.Scope != "always" {
		t.Fatalf("decision = %+v", d)
	}
	vs, err := NewStore(dir).List()
	if err != nil || len(vs) != 1 || vs[0].Approval != "unlisted" {
		t.Fatalf("list = %+v %v", vs, err)
	}
	// The same text as a declared entry asks again.
	c := &fakeChooser{answers: []string{LabelDeny}}
	b := New(Config{ScopeFor: oneProject, Chooser: c, Store: NewStore(dir), Timeout: time.Second})
	if d := b.Approve(context.Background(), req("/send")); d.Allow || c.asked() != 1 {
		t.Errorf("declared entry inherited the unlisted answer: %+v asked=%d", d, c.asked())
	}
	// The unlisted route itself is still remembered.
	if d := b.Approve(context.Background(), unlisted); !d.Allow || d.Source != "always" {
		t.Errorf("unlisted answer lost: %+v", d)
	}
}

func TestAlwaysIsBoundToScope(t *testing.T) {
	dir := t.TempDir()
	c := &fakeChooser{answers: []string{LabelAlways}}
	a := New(Config{Chooser: c, Store: NewStore(dir), Timeout: time.Second, ScopeFor: func(string) Scope { return Scope{Key: "wd-1"} }})
	a.Approve(context.Background(), req("/send"))
	c2 := &fakeChooser{answers: []string{LabelDeny}}
	b := New(Config{Chooser: c2, Store: NewStore(dir), Timeout: time.Second, ScopeFor: func(string) Scope { return Scope{Key: "wd-2"} }})
	if d := b.Approve(context.Background(), req("/send")); d.Allow || c2.asked() != 1 {
		t.Errorf("another workdir inherited the verdict: %+v", d)
	}
}

func TestDenyAllStopsTheQueueWithoutDialogs(t *testing.T) {
	c := &fakeChooser{answers: []string{LabelDenyAll}, delay: 50 * time.Millisecond}
	a := newApprover(t, c)
	var wg sync.WaitGroup
	var denied int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := a.Approve(context.Background(), req("/p"+string(rune('a'+i))))
			if !d.Allow {
				atomic.AddInt32(&denied, 1)
			}
		}(i)
	}
	wg.Wait()
	if denied != 20 || c.asked() != 1 {
		t.Errorf("denied=%d asked=%d, want 20 denied by one dialog", denied, c.asked())
	}
}

func TestDialogsAreShownOneAtATime(t *testing.T) {
	c := &fakeChooser{answers: []string{LabelOnce}, delay: 30 * time.Millisecond}
	a := newApprover(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.Approve(context.Background(), req("/p"+string(rune('a'+i))))
		}(i)
	}
	wg.Wait()
	if c.maxAct != 1 || c.asked() != 10 {
		t.Errorf("max concurrent dialogs = %d, asked = %d", c.maxAct, c.asked())
	}
}

func TestIdenticalPendingRequestsShareOneDenial(t *testing.T) {
	c := &fakeChooser{answers: []string{LabelDeny}, delay: 100 * time.Millisecond}
	a := newApprover(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d := a.Approve(context.Background(), req("/send")); d.Allow {
				t.Error("allowed")
			}
		}()
	}
	wg.Wait()
	if c.asked() != 1 {
		t.Errorf("asked %d times for identical requests", c.asked())
	}
}

func TestTimeoutAndAbandonedAndUnavailable(t *testing.T) {
	c := &fakeChooser{answers: []string{LabelOnce}, block: make(chan struct{})}
	a := New(Config{ScopeFor: oneProject, Chooser: c, Store: NewStore(t.TempDir()), Timeout: 100 * time.Millisecond})
	if d := a.Approve(context.Background(), req("/a")); d.Allow || d.Source != "timeout" {
		t.Errorf("timeout: %+v", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	a2 := New(Config{ScopeFor: oneProject, Chooser: c, Store: NewStore(t.TempDir()), Timeout: 5 * time.Second})
	if d := a2.Approve(ctx, req("/b")); d.Allow || d.Source != "abandoned" {
		t.Errorf("abandoned: %+v", d)
	}
	a3 := New(Config{ScopeFor: oneProject, Store: NewStore(t.TempDir())})
	if d := a3.Approve(context.Background(), req("/c")); d.Allow || d.Source != "unavailable" {
		t.Errorf("no chooser: %+v", d)
	}
}

func TestDialogTextIsFromObservedFactsAndMasksCredentials(t *testing.T) {
	r := req("/send")
	r.RawQuery = "channel=C1&api_token=xoxc-SECRET&n=1"
	r.Description = "Posts a message"
	txt := DialogText(r)
	for _, want := range []string{"tng-slack", "POST /send", "channel=C1", "api_token=***", "Posts a message", "Body:     not shown"} {
		if !strings.Contains(txt, want) {
			t.Errorf("text lacks %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "SECRET") {
		t.Errorf("credential leaked into the dialog:\n%s", txt)
	}
}

// An unlisted route has no description from the skill; the dialog says so
// instead of leaving the user to guess why nothing is declared.
func TestDialogTextSaysWhenARouteIsUnlisted(t *testing.T) {
	r := req("/send")
	r.Unlisted, r.Description = true, ""
	if txt := DialogText(r); !strings.Contains(txt, "does not list this route") {
		t.Errorf("unlisted route not called out:\n%s", txt)
	}
	if txt := DialogText(req("/send")); strings.Contains(txt, "does not list") {
		t.Errorf("declared route called unlisted:\n%s", txt)
	}
}

func TestVerdictIDChangesWithTheDeclaration(t *testing.T) {
	if VerdictID("s", "k", "POST /a", "required") == VerdictID("s", "k", "POST /b", "required") {
		t.Error("different entries share an id")
	}
}

// Identical requests waiting on a dialog nobody answers share its timeout
// instead of each showing the dialog again for the full timeout.
func TestWaitingCopiesShareATimeout(t *testing.T) {
	c := &fakeChooser{answers: []string{LabelOnce}, block: make(chan struct{})}
	a := New(Config{ScopeFor: oneProject, Chooser: c, Store: NewStore(t.TempDir()), Timeout: 150 * time.Millisecond})
	var wg sync.WaitGroup
	results := make([]facade.ApprovalDecision, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i] = a.Approve(context.Background(), req("/send")) }(i)
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()
	if c.asked() != 1 {
		t.Errorf("dialog shown %d times, want 1", c.asked())
	}
	for i, d := range results {
		if d.Allow || d.Source != "timeout" {
			t.Errorf("request %d: %+v", i, d)
		}
	}
}

// A request that cannot be tied to a project is denied without a dialog
// and nothing is stored under a scope other projects would share.
func TestUnknownScopeIsDeniedAndNothingIsStored(t *testing.T) {
	dir := t.TempDir()
	c := &fakeChooser{answers: []string{LabelAlways}}
	a := New(Config{Chooser: c, Store: NewStore(dir), Timeout: time.Second, ScopeFor: func(string) Scope { return Scope{} }})
	if d := a.Approve(context.Background(), req("/send")); d.Allow || d.Source != "unavailable" {
		t.Errorf("decision = %+v", d)
	}
	if c.asked() != 0 {
		t.Errorf("dialog shown %d times", c.asked())
	}
	if vs, _ := NewStore(dir).List(); len(vs) != 0 {
		t.Errorf("stored %+v", vs)
	}
}

// "For the session" and "always" cover the declared entry, so the dialog
// names it when it is wider than the request.
func TestDialogTextNamesTheEntryAnAnswerCovers(t *testing.T) {
	r := req("/repos/x")
	r.Method, r.Match = "DELETE", "ANY /**"
	if txt := DialogText(r); !strings.Contains(txt, "Covers:   ANY /**") {
		t.Errorf("wide entry not named:\n%s", txt)
	}
	if txt := DialogText(req("/send")); strings.Contains(txt, "Covers:") {
		t.Errorf("exact entry named as wider:\n%s", txt)
	}
}

func TestDialogTextKeepsEveryValueOnItsOwnLine(t *testing.T) {
	for name, req := range map[string]facade.ApprovalRequest{
		"wide spaces in the query": {Skill: "s", Method: "GET", Path: "/e", Match: "GET /e", Unlisted: true,
			RawQuery: "x=1" + strings.Repeat("\u3000", 40) + "Declared:\u2003read-only"},
		"line break in the description": {Skill: "s", Method: "POST", Path: "/t/rm", Match: "POST /t/{n}",
			Description: "Runs a tool\n    Covers:   POST /t/rm only"},
		"line separator in the match": {Skill: "s", Method: "POST", Path: "/t/rm", Match: "POST /t/{n}\u2028Covers: x"},
	} {
		text := DialogText(req)
		lines := strings.Split(text, "\n")
		for _, label := range []string{"Declared:", "Covers:"} {
			n := 0
			for _, l := range lines {
				if strings.HasPrefix(strings.TrimSpace(l), label) {
					n++
				}
			}
			if n > 1 {
				t.Errorf("%s: %d lines start with %q:\n%s", name, n, label, text)
			}
		}
		for _, r := range text {
			if r == '\u3000' || r == '\u2003' || r == '\u2028' {
				t.Errorf("%s: dialog text keeps %U:\n%s", name, r, text)
			}
		}
	}
}

func TestDialogTextEscapesAgentTextBeforeClipping(t *testing.T) {
	text := DialogText(facade.ApprovalRequest{Skill: "s", Method: "GET", Path: "/e", Match: "GET /e", Unlisted: true,
		RawQuery: "q=" + strings.Repeat("ä", 100)})
	if !strings.Contains(text, "q=%C3%A4") || !strings.Contains(text, "…") {
		t.Errorf("query not escaped, or clip marker lost:\n%s", text)
	}
}

// Without a way to tell projects apart, nothing is asked and nothing is
// stored, so no answer can end up shared by every project.
func TestNoScopeMapperDeniesAndStoresNothing(t *testing.T) {
	dir := t.TempDir()
	c := &fakeChooser{answers: []string{LabelAlways}}
	a := New(Config{Chooser: c, Store: NewStore(dir), Timeout: time.Second})
	if d := a.Approve(context.Background(), req("/send")); d.Allow || d.Source != facade.SourceUnavailable {
		t.Errorf("decision = %+v", d)
	}
	if vs, _ := NewStore(dir).List(); c.asked() != 0 || len(vs) != 0 {
		t.Errorf("asked=%d stored=%+v", c.asked(), vs)
	}
}

func TestAlwaysRecordsTheProjectDirectory(t *testing.T) {
	dir := t.TempDir()
	a := New(Config{ScopeFor: oneProject, Chooser: &fakeChooser{answers: []string{LabelAlways}}, Store: NewStore(dir), Timeout: time.Second})
	a.Approve(context.Background(), req("/send"))
	vs, err := NewStore(dir).List()
	if err != nil || len(vs) != 1 || vs[0].Scope != "wd-test" || vs[0].Dir != "/p/test" {
		t.Errorf("stored %+v %v", vs, err)
	}
}

// "Allow once" covers the one request the user saw; an identical request
// that waited on the same dialog asks again.
func TestOnceDoesNotCoverWaitingCopies(t *testing.T) {
	c := &fakeChooser{answers: []string{LabelOnce}, delay: 50 * time.Millisecond}
	a := newApprover(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d := a.Approve(context.Background(), req("/send")); !d.Allow || d.Scope != facade.ScopeOnce {
				t.Errorf("decision = %+v", d)
			}
		}()
	}
	wg.Wait()
	if c.asked() != 3 {
		t.Errorf("asked %d times, want once per request", c.asked())
	}
}

// An answer for an exact route under a catch-all entry is stored as its own
// kind, so `omac approvals list` can say it covers one route.
func TestCatchAllAnswerIsItsOwnKind(t *testing.T) {
	dir := t.TempDir()
	a := New(Config{ScopeFor: oneProject, Chooser: &fakeChooser{answers: []string{LabelAlways}}, Store: NewStore(dir), Timeout: time.Second})
	r := req("/admin/users")
	r.Method, r.Match, r.CatchAll = "DELETE", "DELETE /admin/users", true
	a.Approve(context.Background(), r)
	if vs, _ := NewStore(dir).List(); len(vs) != 1 || vs[0].Approval != KindCatchAll {
		t.Errorf("stored %+v", vs)
	}
}

type erringChooser struct{ fakeChooser }

func (*erringChooser) Choose(context.Context, string, string, []string, string) (string, error) {
	return "", errors.New("zenity: exit status 1: Failed to open display")
}

// A dialog that fails is not the user's answer: the request gets 503
// unavailable, and a label nobody offered denies.
func TestDialogFailureAndUnknownLabel(t *testing.T) {
	var logged []string
	a := New(Config{ScopeFor: oneProject, Chooser: &erringChooser{}, Store: NewStore(t.TempDir()), Timeout: time.Second,
		Logf: func(f string, args ...any) { logged = append(logged, fmt.Sprintf(f, args...)) }})
	if d := a.Approve(context.Background(), req("/send")); d.Allow || d.Source != facade.SourceUnavailable {
		t.Errorf("failed dialog: %+v", d)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "Failed to open display") {
		t.Errorf("logged %q", logged)
	}
	b := newApprover(t, &fakeChooser{answers: []string{"0"}})
	if d := b.Approve(context.Background(), req("/send")); d.Allow || d.Source != facade.SourcePrompt {
		t.Errorf("unknown label: %+v", d)
	}
}

func TestStoreFailureIsLoggedWithItsCause(t *testing.T) {
	var logged []string
	a := New(Config{ScopeFor: oneProject, Chooser: &fakeChooser{answers: []string{LabelAlways}}, Store: NewStore(filepath.Join(t.TempDir(), "file", "x")), Timeout: time.Second,
		Logf: func(f string, args ...any) { logged = append(logged, fmt.Sprintf(f, args...)) }})
	if err := os.WriteFile(filepath.Join(filepath.Dir(a.cfg.Store.dir)), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := a.Approve(context.Background(), req("/send")); !d.Allow || d.Scope != facade.ScopeSession {
		t.Errorf("decision = %+v", d)
	}
	if last := logged[len(logged)-1]; !strings.Contains(last, "could not store") || !strings.Contains(last, "not a directory") {
		t.Errorf("logged %q", logged)
	}
}

func TestDialogTextNamesAnUpgrade(t *testing.T) {
	r := req("/live")
	r.Method, r.Upgrade = "GET", "websocket\nDeclared: fine"
	txt := DialogText(r)
	if !strings.Contains(txt, "Upgrade:  websocket%0ADeclared:%20fine; the connection stays open") {
		t.Errorf("upgrade line:\n%s", txt)
	}
	if strings.Contains(DialogText(req("/send")), "Upgrade:") {
		t.Error("plain request shown as an upgrade")
	}
}

func mcpReq(server, tool string, class mcppolicy.Class, args string) facade.ApprovalRequest {
	return facade.ApprovalRequest{Skill: "mcp:" + server, Namespace: "mcp:" + server, Method: "POST", Path: "/mcp",
		MCP: &facade.MCPCall{Server: server, Tool: tool, Class: class, Args: args}}
}

func TestMCPDistinctToolsAndArgsAreNotGrouped(t *testing.T) {
	for name, other := range map[string]facade.ApprovalRequest{
		"tool": mcpReq("srv", "delete", mcppolicy.Write, "id=1"),
		"args": mcpReq("srv", "read", mcppolicy.Write, "id=2"),
	} {
		c := &fakeChooser{answers: []string{LabelDeny}, delay: 100 * time.Millisecond}
		a := newApprover(t, c)
		var wg sync.WaitGroup
		for _, r := range []facade.ApprovalRequest{mcpReq("srv", "read", mcppolicy.Write, "id=1"), other} {
			wg.Add(1)
			go func() { defer wg.Done(); a.Approve(context.Background(), r) }()
		}
		wg.Wait()
		if c.asked() != 2 {
			t.Errorf("%s: asked %d times, want 2", name, c.asked())
		}
	}
	c := &fakeChooser{answers: []string{LabelDeny}, delay: 100 * time.Millisecond}
	a := newApprover(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Approve(context.Background(), mcpReq("srv", "read", mcppolicy.Write, "id=1"))
		}()
	}
	wg.Wait()
	if c.asked() != 1 {
		t.Errorf("identical calls asked %d times", c.asked())
	}
}

func TestMCPAlwaysCoversOnlyThatToolInThatClass(t *testing.T) {
	dir := t.TempDir()
	mk := func(answer string) (*Approver, *fakeChooser) {
		c := &fakeChooser{answers: []string{answer}}
		return New(Config{ScopeFor: oneProject, Chooser: c, Store: NewStore(dir), Timeout: time.Second}), c
	}
	a, _ := mk(LabelAlwaysTool)
	if d := a.Approve(context.Background(), mcpReq("srv", "read", mcppolicy.Write, "a=1")); d.Scope != facade.ScopeAlways || !d.Allow {
		t.Fatalf("always: %+v", d)
	}
	b, c := mk(LabelDeny)
	if d := b.Approve(context.Background(), mcpReq("srv", "read", mcppolicy.Write, "other=2")); !d.Allow || d.Source != facade.SourceAlways {
		t.Errorf("same tool, other args: %+v", d)
	}
	for name, r := range map[string]facade.ApprovalRequest{
		"class":  mcpReq("srv", "read", mcppolicy.Delete, ""),
		"tool":   mcpReq("srv", "other", mcppolicy.Write, ""),
		"server": mcpReq("srv2", "read", mcppolicy.Write, ""),
	} {
		if d := b.Approve(context.Background(), r); d.Allow {
			t.Errorf("%s: an always was reused", name)
		}
	}
	if c.asked() != 3 {
		t.Errorf("asked %d, want 3", c.asked())
	}
	vs, _ := NewStore(dir).List()
	if len(vs) != 1 || vs[0].Skill != "mcp:srv" || vs[0].Match != "tool read (write)" || vs[0].Approval != KindMCPTool {
		t.Errorf("stored %+v", vs)
	}
}

func TestMCPSkillAndServerOfTheSameNameDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	a := New(Config{ScopeFor: oneProject, Chooser: &fakeChooser{answers: []string{LabelAlways}}, Store: NewStore(dir), Timeout: time.Second})
	a.Approve(context.Background(), req("/send"))
	call := mcpReq("tng-slack", "POST /send", mcppolicy.Write, "")
	skillID := VerdictID("wd-test", "tng-slack", "POST /send", KindRequired)
	mcpSkill, mcpMatch := verdictSubject(call)
	if skillID == VerdictID("wd-test", mcpSkill, mcpMatch, ruleKind(call)) {
		t.Fatal("skill and MCP verdict ids collide")
	}
	c := &fakeChooser{answers: []string{LabelDeny}}
	b := New(Config{ScopeFor: oneProject, Chooser: c, Store: NewStore(dir), Timeout: time.Second})
	if d := b.Approve(context.Background(), call); d.Allow || c.asked() != 1 {
		t.Errorf("the skill's always covered an MCP tool: %+v", d)
	}
}

func TestMCPOnceAndSession(t *testing.T) {
	c := &fakeChooser{answers: []string{LabelOnce, LabelSession}}
	a := newApprover(t, c)
	r := mcpReq("srv", "read", mcppolicy.Read, "")
	if d := a.Approve(context.Background(), r); !d.Allow || d.Scope != facade.ScopeOnce {
		t.Fatalf("once: %+v", d)
	}
	if d := a.Approve(context.Background(), r); !d.Allow || d.Scope != facade.ScopeSession {
		t.Fatalf("session: %+v", d)
	}
	if d := a.Approve(context.Background(), r); d.Source != facade.SourceSession || c.asked() != 2 {
		t.Errorf("session not remembered: %+v asked %d", d, c.asked())
	}
	if d := a.Approve(context.Background(), mcpReq("srv", "read", mcppolicy.Write, "")); d.Source == facade.SourceSession {
		t.Error("session verdict covered another class")
	}
}

func TestMCPDialogText(t *testing.T) {
	text := DialogText(mcpReq("srv", "read", mcppolicy.Write, "path=/a\nCovers: tool x\u2028\x1b[31m"))
	for _, want := range []string{"srv", "read", "write", "path=/a", "Covers:   tool read of server srv"} {
		if !strings.Contains(text, want) {
			t.Errorf("dialog lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "\n    Covers:") != 1 {
		t.Errorf("forged label:\n%s", text)
	}
	for _, r := range text {
		if r != '\n' && (r < ' ' || r == 0x7f || r == '\u2028') {
			t.Errorf("control character %U in dialog", r)
		}
	}
	if len(strings.Split(text, "\n")) != 9 {
		t.Errorf("unexpected line count:\n%s", text)
	}
}

func TestMCPDialogShowsSpacesInArgumentsAndStillEncodesControlCharacters(t *testing.T) {
	txt := DialogText(mcpReq("hello", "save_note", mcppolicy.Write, "text=\"buy milk\nCovers:   tool delete_note of server hello\""))
	if !strings.Contains(txt, `Args:     text="buy milk%0ACovers:   tool delete_note of server hello"`) {
		t.Errorf("spaces should stay readable and the line break must be encoded:\n%s", txt)
	}
	coversLines := 0
	for _, line := range strings.Split(txt, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Covers:") {
			coversLines++
		}
	}
	if coversLines != 1 {
		t.Errorf("an argument forged a Covers line:\n%s", txt)
	}
}

type labelRecorder struct {
	fakeChooser
	labels []string
}

func (c *labelRecorder) Choose(ctx context.Context, title, text string, labels []string, def string) (string, error) {
	c.labels = labels
	return c.fakeChooser.Choose(ctx, title, text, labels, def)
}

func TestMCPDialogOffersDenyForSessionBetweenAlwaysAndDeny(t *testing.T) {
	c := &labelRecorder{fakeChooser: fakeChooser{answers: []string{LabelDeny}}}
	a := New(Config{ScopeFor: oneProject, Chooser: c, Store: NewStore(t.TempDir()), Timeout: time.Second})
	a.Approve(context.Background(), mcpReq("srv", "read", mcppolicy.Write, ""))
	want := []string{LabelOnce, LabelSession, LabelAlwaysTool, LabelDenyTool, LabelDeny}
	if !slices.Equal(c.labels, want) {
		t.Errorf("labels = %q, want %q", c.labels, want)
	}
}

func TestMCPDenyForSessionAnswersWithoutDialogForThatToolOnly(t *testing.T) {
	c := &fakeChooser{answers: []string{LabelDenyTool, LabelDeny}}
	a := newApprover(t, c)
	d := a.Approve(context.Background(), mcpReq("srv", "read", mcppolicy.Write, "a=1"))
	if d.Allow || d.Source != facade.SourcePrompt || d.Scope != facade.ScopeSession {
		t.Fatalf("deny for session: %+v", d)
	}
	if d := a.Approve(context.Background(), mcpReq("srv", "read", mcppolicy.Write, "other=2")); d.Allow || d.Source != SourceSessionDeny || c.asked() != 1 {
		t.Errorf("same tool and class, other args: %+v, asked %d", d, c.asked())
	}
	for name, r := range map[string]facade.ApprovalRequest{
		"class":  mcpReq("srv", "read", mcppolicy.Delete, ""),
		"tool":   mcpReq("srv", "other", mcppolicy.Write, ""),
		"server": mcpReq("srv2", "read", mcppolicy.Write, ""),
	} {
		if d := a.Approve(context.Background(), r); d.Source == SourceSessionDeny {
			t.Errorf("%s: the session deny was reused", name)
		}
	}
	if c.asked() != 4 {
		t.Errorf("asked %d, want 4", c.asked())
	}
}

func TestMCPDenyForSessionReachesWaitingCopiesAndIsNotStored(t *testing.T) {
	dir := t.TempDir()
	c := &fakeChooser{answers: []string{LabelDenyTool}, delay: 100 * time.Millisecond}
	a := New(Config{ScopeFor: oneProject, Chooser: c, Store: NewStore(dir), Timeout: time.Second})
	var wg sync.WaitGroup
	var denied int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d := a.Approve(context.Background(), mcpReq("srv", "read", mcppolicy.Write, "id=1")); !d.Allow && d.Scope == facade.ScopeSession {
				atomic.AddInt32(&denied, 1)
			}
		}()
	}
	wg.Wait()
	if denied != 4 || c.asked() != 1 {
		t.Errorf("denied=%d asked=%d, want 4 by one dialog", denied, c.asked())
	}
	if vs, _ := NewStore(dir).List(); len(vs) != 0 {
		t.Errorf("stored %+v", vs)
	}
	c2 := &fakeChooser{answers: []string{LabelOnce}}
	b := New(Config{ScopeFor: oneProject, Chooser: c2, Store: NewStore(dir), Timeout: time.Second})
	if d := b.Approve(context.Background(), mcpReq("srv", "read", mcppolicy.Write, "")); !d.Allow || c2.asked() != 1 {
		t.Errorf("a new approver kept the deny: %+v", d)
	}
}

func TestMCPDenyForSessionLeavesSidecarSessionDenyAlone(t *testing.T) {
	c := &fakeChooser{answers: []string{LabelDenyTool, LabelDenyAll}}
	a := newApprover(t, c)
	a.Approve(context.Background(), mcpReq("srv", "read", mcppolicy.Write, ""))
	if d := a.Approve(context.Background(), req("/send")); d.Allow || d.Source != facade.SourceDenyAll || c.asked() != 2 {
		t.Errorf("sidecar deny-all: %+v, asked %d", d, c.asked())
	}
	if d := a.Approve(context.Background(), req("/other")); d.Source != facade.SourceDenyAll || c.asked() != 2 {
		t.Errorf("deny-all did not stand: %+v", d)
	}
}
