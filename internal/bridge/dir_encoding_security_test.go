package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// How the bridge hooks name a directory.
//
// Each harness that is not OpenCode is wired to omac by a shell hook that
// posts the session's working directory to the control plane, which then
// activates that directory: mounts its skills, mints its access token, and
// hands the session the manifest. The directory is the whole of the request —
// it decides which project's skills the session gets.
//
// The harness hands the hook a JSON payload and the hook reads the directory
// out of it with jq, correctly. It then builds its own request by pasting that
// path straight into a JSON string, `{"dir":"${dir}"}`. A path is a byte string
// that may contain a double quote, and one closes the JSON string early. The
// rest of the path is read as JSON rather than as a path, and a second "dir"
// key appended that way wins, because a decoder keeps the last occurrence of a
// duplicate field.
//
// The path is not something omac chooses. It is the directory the session was
// started in, so it is attacker-chosen whenever a user is induced to open a
// prepared directory — the same premise as cloning a hostile repository, which
// omac otherwise handles by confining it.

// hooks are the harness bridge scripts and the hook event each treats as
// "session starting", which is the branch that activates a directory.
var hooks = []struct {
	name       string
	path       string
	startEvent string
}{
	{"claude", "../../.claude/hooks/omac-bridge.sh", "SessionStart"},
	{"codex", "../../.codex/hooks/omac-bridge.sh", "SessionStart"},
	{"copilot", "../../.copilot/hooks/omac-bridge.sh", "SessionStart"},
}

// controlStub records the directory each request resolves to, decoded the way
// the control plane decodes it.
type controlStub struct {
	mu   sync.Mutex
	dirs []string
	raw  []string
}

func (c *controlStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, 4096)
	n, _ := r.Body.Read(body)
	raw := string(body[:n])

	var req struct {
		Dir string `json:"dir"`
	}
	_ = json.Unmarshal([]byte(raw), &req)

	c.mu.Lock()
	c.dirs = append(c.dirs, req.Dir)
	c.raw = append(c.raw, raw)
	c.mu.Unlock()

	_, _ = w.Write([]byte(`{"dir":"","dir_token":"","state":"active","skills":[]}`))
}

func (c *controlStub) seen() ([]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirs, c.raw
}

// runHook executes one bridge script with a well-formed harness payload. The
// payload is built with a real JSON encoder, so the hostile directory reaches
// the hook exactly as a harness would deliver it and any breakage below is the
// hook's own encoding, not the fixture's.
func runHook(t *testing.T, script, event, dir, controlBase string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"hook_event_name": event,
		"cwd":             dir,
		"session_id":      "security-suite",
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Stdin = strings.NewReader(string(payload))
	// Built explicitly rather than appended to os.Environ(): an ambient
	// OMAC_CONTROL_BASE (every shell inside an omac session has one) would
	// otherwise appear twice, and a shell that prefers the first entry would
	// aim this hostile payload at a live control plane.
	cmd.Env = []string{
		"OMAC_CONTROL_BASE=" + controlBase,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook %s failed: %v\n%s", script, err, out)
	}
}

// TestSecurityBridgeHookEncodesSessionDirectory asserts that a directory whose
// name contains JSON punctuation is transmitted as that directory and nothing
// else.
func TestSecurityBridgeHookEncodesSessionDirectory(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	for _, tool := range []string{"bash", "curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required to exercise the bridge hooks: %v", tool, err)
		}
	}

	// A directory name that closes the JSON string and appends its own "dir".
	// Legal on every filesystem omac supports.
	const hostile = `/tmp/omac-security-probe","dir":"/etc`
	const plainDir = "/tmp/omac-security-probe"

	for _, h := range hooks {
		t.Run(h.name, func(t *testing.T) {
			if _, err := os.Stat(h.path); err != nil {
				t.Fatalf("bridge hook not found at %s: %v", h.path, err)
			}

			// Control: an ordinary path arrives unchanged, which proves the
			// hook reached the control plane at all. Without it a hook that
			// silently did nothing would satisfy the assertion below.
			plain := &controlStub{}
			plainSrv := httptest.NewServer(plain)
			defer plainSrv.Close()
			runHook(t, h.path, h.startEvent, plainDir, plainSrv.URL)
			if dirs, _ := plain.seen(); len(dirs) == 0 || dirs[0] != plainDir {
				t.Fatalf("the hook did not post an ordinary directory (%v): the fixture is broken, not the security property", dirs)
			}

			stub := &controlStub{}
			srv := httptest.NewServer(stub)
			defer srv.Close()
			runHook(t, h.path, h.startEvent, hostile, srv.URL)

			dirs, raw := stub.seen()
			if len(dirs) == 0 {
				t.Fatal("the hook posted nothing for the hostile directory")
			}
			if dirs[0] != hostile {
				t.Errorf("the control plane read the session directory as %q instead of %q (body was %s): a directory name decides which project the session is given, and this one names a different directory than the session is in",
					dirs[0], hostile, fmt.Sprintf("%q", raw[0]))
			}
		})
	}
}

// The same hooks, second property: what they render, not what they post.
//
// The manifest the control plane returns names the session directory, and
// each hook renders that name into the block of instructions it injects on
// omac's behalf (hookSpecificOutput.additionalContext). The skill fields in
// that block pass through sanitize_field precisely so untrusted content
// cannot shape omac's instructions; the directory name needs the same
// treatment, because it is just as attacker-chosen as a skill field.

// dirEchoStub answers the activate request the way the control plane's
// manifestFor does: it hands the posted directory straight back in the
// manifest, with one ready global skill so the renderer also emits the
// global-skills instruction. The name is served exactly as received on
// purpose — what the renderer does with it is the property under test.
type dirEchoStub struct {
	mu  sync.Mutex
	dir string
}

func (s *dirEchoStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir string `json:"dir"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	s.dir = req.Dir
	s.mu.Unlock()

	manifest := map[string]any{
		"dir":   req.Dir,
		"state": "active",
		"skills": []map[string]any{{
			"name":  "demo-global",
			"scope": "global",
			"mount": "demo-global",
			"state": "ready",
			"base":  "http://127.0.0.1:1/demo-global",
		}},
	}
	_ = json.NewEncoder(w).Encode(manifest)
}

// runHookStdout runs one bridge hook like runHook and returns its stdout,
// which carries the hookSpecificOutput JSON the harness turns into agent
// context.
func runHookStdout(t *testing.T, script, event, dir, controlBase string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"hook_event_name": event,
		"cwd":             dir,
		"session_id":      "security-suite",
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Stdin = strings.NewReader(string(payload))
	// Same explicit environment as runHook: an ambient OMAC_CONTROL_BASE must
	// not aim this payload at a live control plane.
	cmd.Env = []string{
		"OMAC_CONTROL_BASE=" + controlBase,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook %s failed: %v", script, err)
	}
	return stdout.String()
}

// injectedContext decodes the additionalContext a SessionStart hook emits:
// the text the harness adds to the agent's context as omac-provided
// instructions.
func injectedContext(t *testing.T, hookStdout string) string {
	t.Helper()
	if strings.TrimSpace(hookStdout) == "" {
		t.Fatal("the hook emitted no context: the fixture is broken, not the security property")
	}
	var out struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(hookStdout), &out); err != nil {
		t.Fatalf("hook stdout is not the expected hookSpecificOutput JSON: %v\n%s", err, hookStdout)
	}
	return out.HookSpecificOutput.AdditionalContext
}

// TestSecurityBridgeHookSanitizesSessionDirectory asserts that a directory
// name cannot shape the instruction block a bridge hook injects: the name is
// rendered through the field sanitizer, so a line break in it adds no line
// to the block and a quote in it leaves the JSON sample the block hands the
// agent in its intended shape.
func TestSecurityBridgeHookSanitizesSessionDirectory(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	for _, tool := range []string{"bash", "curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required to exercise the bridge hooks: %v", tool, err)
		}
	}

	// A legal directory name with a line break and a quote in it. The
	// probes make the two effects observable; the name is otherwise
	// arbitrary.
	const probeLine = "probe-second-line"
	const probeKey = "probe-json-key"
	hostile := "/tmp/omac-security-probe\n" + probeLine + `","` + probeKey + `":"1"`

	for _, h := range hooks {
		t.Run(h.name, func(t *testing.T) {
			if _, err := os.Stat(h.path); err != nil {
				t.Fatalf("bridge hook not found at %s: %v", h.path, err)
			}
			stub := &dirEchoStub{}
			srv := httptest.NewServer(stub)
			defer srv.Close()

			ctx := injectedContext(t, runHookStdout(t, h.path, h.startEvent, hostile, srv.URL))

			stub.mu.Lock()
			posted := stub.dir
			stub.mu.Unlock()
			if posted != hostile {
				t.Fatalf("the hook did not deliver the probe directory to the stub (%q): the fixture is broken, not the security property", posted)
			}

			// A line break in the name must not contribute a standalone
			// line to the injected block.
			for _, ln := range strings.Split(ctx, "\n") {
				if strings.HasPrefix(strings.TrimSpace(ln), probeLine) {
					t.Errorf("%s: a line break in the directory name added a standalone line to the injected block:\n%s\n"+
						"The block is instructions the harness trusts as omac's own, and the name is attacker-chosen whenever a user opens a prepared directory.",
						h.path, ctx)
					break
				}
			}

			// A quote in the name must not change the shape of the JSON
			// sample the block tells the agent to copy into a skill request
			// body: within the sample, the name is rendered with the
			// characters that would change the sample's shape removed.
			const samplePrefix = `"target_path": "`
			site := strings.Index(ctx, samplePrefix)
			if site < 0 {
				t.Fatalf("%s: the injected block has no target_path sample: the fixture is broken, not the security property", h.path)
			}
			rest := ctx[site+len(samplePrefix):]
			end := strings.Index(rest, `/skills"`)
			if end < 0 {
				t.Fatalf("%s: the target_path sample is not terminated by the skills directory: the fixture is broken, not the security property", h.path)
			}
			if strings.ContainsAny(rest[:end], "\"\n") {
				t.Errorf("%s: a quote in the directory name changed the shape of the JSON sample in the injected block:\n%s\n"+
					"The sample must keep its single-key shape whatever characters the directory name contains.",
					h.path, ctx)
			}
		})
	}
}
