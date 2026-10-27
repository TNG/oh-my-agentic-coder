package approver

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/facade"
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

func newApprover(t *testing.T, c *fakeChooser) *Approver {
	t.Helper()
	return New(Config{Chooser: c, Store: NewStore(t.TempDir()), Timeout: 2 * time.Second})
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
	a := New(Config{Chooser: c, Store: NewStore(dir), Timeout: time.Second})
	if d := a.Approve(context.Background(), req("/send")); !d.Allow || d.Scope != "always" {
		t.Fatalf("decision = %+v", d)
	}
	c2 := &fakeChooser{answers: []string{LabelDeny}}
	a2 := New(Config{Chooser: c2, Store: NewStore(dir), Timeout: time.Second})
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

func TestAlwaysIsBoundToScope(t *testing.T) {
	dir := t.TempDir()
	c := &fakeChooser{answers: []string{LabelAlways}}
	a := New(Config{Chooser: c, Store: NewStore(dir), Timeout: time.Second, ScopeFor: func(string) string { return "wd-1" }})
	a.Approve(context.Background(), req("/send"))
	c2 := &fakeChooser{answers: []string{LabelDeny}}
	b := New(Config{Chooser: c2, Store: NewStore(dir), Timeout: time.Second, ScopeFor: func(string) string { return "wd-2" }})
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
	a := New(Config{Chooser: c, Store: NewStore(t.TempDir()), Timeout: 100 * time.Millisecond})
	if d := a.Approve(context.Background(), req("/a")); d.Allow || d.Source != "timeout" {
		t.Errorf("timeout: %+v", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	a2 := New(Config{Chooser: c, Store: NewStore(t.TempDir()), Timeout: 5 * time.Second})
	if d := a2.Approve(ctx, req("/b")); d.Allow || d.Source != "abandoned" {
		t.Errorf("abandoned: %+v", d)
	}
	a3 := New(Config{Store: NewStore(t.TempDir())})
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

func TestVerdictIDChangesWithTheDeclaration(t *testing.T) {
	if VerdictID("s", "k", "POST /a", "required") == VerdictID("s", "k", "POST /b", "required") {
		t.Error("different entries share an id")
	}
}
