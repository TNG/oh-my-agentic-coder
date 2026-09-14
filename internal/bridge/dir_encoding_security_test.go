//go:build vuln

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
// The hooks build the request body by pasting the path straight into a JSON
// string, `{"dir":"${dir}"}`. A path is a byte string that may contain a
// double quote, and one closes the JSON string early. The rest of the path is
// then read as JSON rather than as a path, and a second "dir" key appended
// that way wins, because a decoder keeps the last occurrence of a duplicate
// field.
//
// The path is not something omac chooses. It is the directory the session was
// started in, so it is attacker-chosen whenever a user is induced to open a
// prepared directory — the same premise as cloning a hostile repository, which
// omac otherwise handles by confining it.

// hooks are the harness bridge scripts and the env var each reads the session
// directory from when no payload is parsed.
var hooks = []struct {
	name   string
	path   string
	dirEnv string
}{
	{"claude", "../../.claude/hooks/omac-bridge.sh", "CLAUDE_PROJECT_DIR"},
	{"codex", "../../.codex/hooks/omac-bridge.sh", "CODEX_PROJECT_DIR"},
	{"copilot", "../../.copilot/hooks/omac-bridge.sh", "COPILOT_PROJECT_DIR"},
}

// controlStub records the directory each activate request resolves to, decoded
// the way the control plane decodes it.
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

// runHook executes one bridge script with an empty payload, so the script
// falls back to its project-dir environment variable for the session
// directory.
func runHook(t *testing.T, script, dirEnv, dir, controlBase string) {
	t.Helper()
	cmd := exec.Command("bash", script)
	cmd.Stdin = strings.NewReader("")
	cmd.Env = append(os.Environ(),
		"OMAC_CONTROL_BASE="+controlBase,
		dirEnv+"="+dir,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook %s failed: %v\n%s", script, err, out)
	}
}

// TestSecurityBridgeHookEncodesSessionDirectory asserts that a directory whose
// name contains JSON punctuation is transmitted as that directory and nothing
// else.
func TestSecurityBridgeHookEncodesSessionDirectory(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Fatalf("bash is required to run the bridge hooks: %v", err)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Fatalf("curl is required by the bridge hooks: %v", err)
	}

	// A directory name that closes the JSON string and appends its own "dir".
	// Legal on every filesystem omac supports.
	const hostile = `/tmp/omac-security-probe","dir":"/etc`

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
			runHook(t, h.path, h.dirEnv, "/tmp/omac-security-probe", plainSrv.URL)
			if dirs, _ := plain.seen(); len(dirs) == 0 || dirs[0] != "/tmp/omac-security-probe" {
				t.Fatalf("the hook did not post an ordinary directory (%v): the fixture is broken, not the security property", dirs)
			}

			stub := &controlStub{}
			srv := httptest.NewServer(stub)
			defer srv.Close()
			runHook(t, h.path, h.dirEnv, hostile, srv.URL)

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
