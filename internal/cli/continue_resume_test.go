package cli

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/session"
)

// devnullEnv builds an Env whose streams all point at /dev/null. Suitable for
// the opts-building helpers, which never read stdin and only write diagnostics.
func devnullEnv(t *testing.T) *Env {
	t.Helper()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	t.Cleanup(func() { null.Close() })
	return &Env{Version: "test", Workdir: "/w", Stdout: null, Stderr: null, Stdin: null}
}

func TestBuildContinueOpts(t *testing.T) {
	cases := []struct {
		name          string
		args          []string
		wantHarness   string
		wantInnerArgs []string
		wantVerbose   bool
		wantNoSandbox bool
	}{
		{"default harness", nil, "opencode", []string{"--continue"}, false, false},
		{"claude token", []string{"claude"}, "claude-code", []string{"--continue"}, false, false},
		{"start flags preserved", []string{"--verbose", "--no-sandbox"}, "opencode", []string{"--continue"}, true, true},
		{"trailing inner args preserved", []string{"--", "--model", "anthropic/x"}, "opencode", []string{"--continue", "--model", "anthropic/x"}, false, false},
		{"claude with flags and inner", []string{"claude", "--verbose", "--", "--foo"}, "claude-code", []string{"--continue", "--foo"}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts, code := buildContinueOpts(c.args, devnullEnv(t))
			if code != ExitOK {
				t.Fatalf("code = %d, want ExitOK", code)
			}
			if opts.harness.Name != c.wantHarness {
				t.Errorf("harness = %q, want %q", opts.harness.Name, c.wantHarness)
			}
			if !reflect.DeepEqual(opts.innerArgs, c.wantInnerArgs) {
				t.Errorf("innerArgs = %v, want %v", opts.innerArgs, c.wantInnerArgs)
			}
			if opts.verbose != c.wantVerbose {
				t.Errorf("verbose = %v, want %v", opts.verbose, c.wantVerbose)
			}
			if opts.noSandbox != c.wantNoSandbox {
				t.Errorf("noSandbox = %v, want %v", opts.noSandbox, c.wantNoSandbox)
			}
		})
	}
}

func TestBuildResumeInnerArgs(t *testing.T) {
	oc, _ := config.LookupHarness("opencode")
	cc, _ := config.LookupHarness("claude-code")

	if got := buildResumeInnerArgs(oc.Session, "ses_X", nil); !reflect.DeepEqual(got, []string{"--session", "ses_X"}) {
		t.Errorf("opencode resume args = %v, want [--session ses_X]", got)
	}
	if got := buildResumeInnerArgs(cc.Session, "uuid-1", nil); !reflect.DeepEqual(got, []string{"--resume", "uuid-1"}) {
		t.Errorf("claude resume args = %v, want [--resume uuid-1]", got)
	}
	// User-supplied inner args follow the resume flag.
	if got := buildResumeInnerArgs(oc.Session, "ses_X", []string{"--model", "y"}); !reflect.DeepEqual(got, []string{"--session", "ses_X", "--model", "y"}) {
		t.Errorf("resume args with user inner = %v", got)
	}
}

func TestParseSelection(t *testing.T) {
	cases := []struct {
		line    string
		n       int
		wantIdx int
		wantOK  bool
	}{
		{"1", 3, 0, true},
		{"3", 3, 2, true},
		{"  2 ", 3, 1, true},
		{"", 3, 0, false},   // cancel
		{"0", 3, 0, false},  // out of range low
		{"4", 3, 0, false},  // out of range high
		{"x", 3, 0, false},  // non-numeric
		{"-1", 3, 0, false}, // negative
	}
	for _, c := range cases {
		idx, ok := parseSelection(c.line, c.n)
		if idx != c.wantIdx || ok != c.wantOK {
			t.Errorf("parseSelection(%q, %d) = (%d,%v), want (%d,%v)", c.line, c.n, idx, ok, c.wantIdx, c.wantOK)
		}
	}
}

func TestPickSessionNonTTY(t *testing.T) {
	// Stdin from a pipe is not a TTY, so pickSession must print the list and
	// return false without blocking on input.
	r, _, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	null, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	defer null.Close()
	env := &Env{Version: "test", Workdir: "/w", Stdout: null, Stderr: null, Stdin: r}

	sessions := []session.Session{{ID: "a", Title: "first"}, {ID: "b", Title: "second"}}
	if _, ok := pickSession(env, "opencode", sessions); ok {
		t.Error("non-TTY stdin should not yield a selection")
	}
}

func TestRelativeTime(t *testing.T) {
	if got := relativeTime(time.Time{}); got != "unknown" {
		t.Errorf("zero time = %q, want unknown", got)
	}
	if got := relativeTime(time.Now().Add(-2 * time.Hour)); got != "2h ago" {
		t.Errorf("2h ago = %q", got)
	}
	if got := relativeTime(time.Now().Add(-49 * time.Hour)); got != "2d ago" {
		t.Errorf("2d ago = %q", got)
	}
}
