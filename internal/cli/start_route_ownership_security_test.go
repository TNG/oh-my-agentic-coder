package cli

// Who owns a mount that `omac start` brought up.
//
// Start installs a facade route for every skill it spawns at launch. The
// route's Owner field is what lets the facade refuse a later registration
// that claims the same mount (internal/facade, AddRoute). The test below
// asserts that property end to end: the route must carry the owning skill's
// name from the moment the session starts. Without it, a skill registered
// later in the session can replace a live, host-approved skill's route, and
// every request the agent sends to that mount is served by the newcomer's
// placeholder instead.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
)

// stageApprovedSidecarSkill writes a workdir skill whose python sidecar
// really serves, registers it, and records host approval for its current
// on-disk code. This is the state `omac register` plus a user approval
// leave a skill in.
func stageApprovedSidecarSkill(t *testing.T, workdir, name, mount, body string) {
	t.Helper()
	skillDir := filepath.Join(workdir, ".opencode", "skills", name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	meta := "name: " + name + "\n" +
		"type: skill\n" +
		"sidecar:\n" +
		"  command: [\"python3\", \"server.py\"]\n" +
		"  mount: " + mount + "\n" +
		"  health:\n" +
		"    path: /status\n" +
		"    timeout_ms: 20000\n" +
		"    interval_ms: 100\n"
	if err := os.WriteFile(filepath.Join(skillDir, config.MetaFileName), []byte(meta), 0o644); err != nil {
		t.Fatalf("write omac.yaml: %v", err)
	}
	server := "import os\n" +
		"import http.server\n" +
		"import socketserver\n" +
		"# One request at a time is all the health probes and the test's own\n" +
		"# requests need.\n" +
		"class H(http.server.BaseHTTPRequestHandler):\n" +
		"    def do_GET(self):\n" +
		"        self.send_response(200)\n" +
		"        self.end_headers()\n" +
		"        if self.path == \"/status\":\n" +
		"            self.wfile.write(b\"ok\")\n" +
		"            return\n" +
		"        self.wfile.write(b\"" + body + "\")\n" +
		"    def log_message(self, *args):\n" +
		"        pass\n" +
		"socketserver.TCPServer((\"127.0.0.1\", int(os.environ[\"SIDECAR_PORT\"])), H).serve_forever()\n"
	if err := os.WriteFile(filepath.Join(skillDir, "server.py"), []byte(server), 0o644); err != nil {
		t.Fatalf("write server.py: %v", err)
	}

	bundle := bundleHashOf(t, skillDir)
	if err := registry.WithLock(workdir, func() error {
		reg, err := registry.Load(workdir)
		if err != nil {
			return err
		}
		reg.Upsert(registry.Entry{
			Name:         name,
			SkillDir:     filepath.Join(".opencode", "skills", name),
			BundleHash:   bundle,
			RegisteredAt: time.Now().UTC(),
		})
		return registry.Save(workdir, reg)
	}); err != nil {
		t.Fatalf("register skill: %v", err)
	}
	if err := skilltrust.Approve(name, bundle, skillDir); err != nil {
		t.Fatalf("approve skill: %v", err)
	}
}

// registerUnapprovedSkill registers a second workdir skill without
// approving it, which is the state a skill authored during the session is
// in. Its manifest claims the given mount, which another skill may already
// be serving on.
func registerUnapprovedSkill(t *testing.T, workdir, name, mount string) {
	t.Helper()
	skillDir := filepath.Join(workdir, ".opencode", "skills", name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill dir: %v", err)
	}
	meta := "name: " + name + "\n" +
		"type: skill\n" +
		"sidecar:\n" +
		"  command: [\"python3\", \"server.py\"]\n" +
		"  mount: " + mount + "\n"
	if err := os.WriteFile(filepath.Join(skillDir, config.MetaFileName), []byte(meta), 0o644); err != nil {
		t.Fatalf("write omac.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "server.py"), []byte("# never spawned in this test\n"), 0o644); err != nil {
		t.Fatalf("write server.py: %v", err)
	}
	bundle := bundleHashOf(t, skillDir)
	if err := registry.WithLock(workdir, func() error {
		reg, err := registry.Load(workdir)
		if err != nil {
			return err
		}
		reg.Upsert(registry.Entry{
			Name:         name,
			SkillDir:     filepath.Join(".opencode", "skills", name),
			BundleHash:   bundle,
			RegisteredAt: time.Now().UTC(),
		})
		return registry.Save(workdir, reg)
	}); err != nil {
		t.Fatalf("register skill: %v", err)
	}
}

// getThroughFacade fetches url with the session's facade token and returns
// status and body. Transport problems come back as status 0 with the error
// text, so callers can retry while the session is still coming up.
func getThroughFacade(url, token string) (int, string) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("X-Omac-Facade-Token", token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestSecurityStartModeRoutesCarryOwner(t *testing.T) {
	requireWorkingPython3(t)
	sectest.RequireLoopbackListener(t)
	isolateHome(t)
	// A short TMPDIR keeps the control-info file and any sockets the launch
	// pipeline creates inside this test's lifetime, independent of the
	// outer environment.
	shortTmp, err := os.MkdirTemp("/tmp", "omac-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shortTmp) })
	t.Setenv("TMPDIR", shortTmp)
	t.Setenv("XDG_RUNTIME_DIR", shortTmp)

	workdir := t.TempDir()
	const (
		approvedName = "keeper"
		claimedMount = "keeper"
		latecomer    = "latecomer"
		approvedBody = "answer-from-keeper"
	)
	stageApprovedSidecarSkill(t, workdir, approvedName, claimedMount, approvedBody)

	// A hold script plays the launched agent: it dumps its environment and
	// stays alive until the test releases it. The dump carries OMAC_PORT
	// and OMAC_FACADE_TOKEN, the values a started session exposes for its
	// mounts.
	envFile := filepath.Join(t.TempDir(), "inner-env")
	exitFile := filepath.Join(t.TempDir(), "inner-exit")
	t.Setenv("OMAC_TEST_INNER_ENV", envFile)
	t.Setenv("OMAC_TEST_INNER_EXIT", exitFile)
	innerPath := filepath.Join(t.TempDir(), "hold")
	if err := os.WriteFile(innerPath, []byte("#!/bin/sh\nenv > \"$OMAC_TEST_INNER_ENV\"\nwhile [ ! -e \"$OMAC_TEST_INNER_EXIT\" ]; do sleep 0.2; done\n"), 0o700); err != nil {
		t.Fatalf("write inner script: %v", err)
	}
	// Release the hold even when the test fails partway, so runLaunch can
	// finish instead of pinning the package run.
	t.Cleanup(func() { _ = os.WriteFile(exitFile, []byte("done"), 0o644) })

	env, stderrText := launchTestEnv(t, workdir)
	harness, ok := config.LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness missing")
	}
	done := make(chan int, 1)
	go func() {
		done <- runLaunch(env, launchOpts{
			label:            "start",
			harness:          harness,
			innerCmdOverride: innerPath,
			noSandbox:        true,
		})
	}()

	// Wait for the session to come up: the reload endpoint published and
	// the facade port plus token visible to the agent.
	var (
		controlBase  string
		controlToken string
		facadePort   int
		facadeToken  string
	)
	setupDeadline := time.Now().Add(45 * time.Second)
	for {
		if ci, ok := readControlInfo(); ok {
			controlBase, controlToken = ci.ControlBase, ci.ControlToken
		}
		if data, err := os.ReadFile(envFile); err == nil {
			innerEnv := parseEnvironment(string(data))
			if v := innerEnv["OMAC_PORT"]; v != "" {
				if p, err := strconv.Atoi(v); err == nil {
					facadePort = p
				}
			}
			facadeToken = innerEnv["OMAC_FACADE_TOKEN"]
		}
		if controlBase != "" && facadePort != 0 && facadeToken != "" {
			break
		}
		select {
		case code := <-done:
			t.Fatalf("runLaunch returned %d before the session came up\nstderr:\n%s", code, stderrText())
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(setupDeadline) {
			t.Fatalf("session did not come up within 45s (controlBase=%q facadePort=%d)\nstderr:\n%s", controlBase, facadePort, stderrText())
		}
	}
	base := "http://127.0.0.1:" + strconv.Itoa(facadePort)

	// Fixture control: the approved skill's route is up and serving.
	probeURL := base + "/" + claimedMount + "/answer"
	code, body := getThroughFacade(probeURL, facadeToken)
	probeDeadline := time.Now().Add(30 * time.Second)
	for (code != http.StatusOK || body != approvedBody) && time.Now().Before(probeDeadline) {
		select {
		case exitCode := <-done:
			t.Fatalf("runLaunch returned %d before the approved skill served\nstderr:\n%s", exitCode, stderrText())
		case <-time.After(200 * time.Millisecond):
		}
		code, body = getThroughFacade(probeURL, facadeToken)
	}
	if code != http.StatusOK || body != approvedBody {
		t.Fatalf("fixture: /%s/answer via the facade is %d %q, want 200 %q\nstderr:\n%s", claimedMount, code, body, approvedBody, stderrText())
	}

	// Mid-session, a second skill is registered without approval, claiming
	// the same mount. Reload then re-reads the workdir registry. Its
	// response is the skill manifest, which is the fixture control: the
	// latecomer must appear there at the claimed mount in the
	// not-approved state, whatever the facade does with its route.
	registerUnapprovedSkill(t, workdir, latecomer, claimedMount)
	req, err := http.NewRequest(http.MethodPost, controlBase+"/__omac__/reload", nil)
	if err != nil {
		t.Fatalf("build reload request: %v", err)
	}
	req.Header.Set("X-Omac-Control-Token", controlToken)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST reload: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST reload: status %d, body %q", resp.StatusCode, string(respBody))
	}
	var manifest struct {
		Skills []struct {
			Name  string `json:"name"`
			Mount string `json:"mount"`
			State string `json:"state"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(respBody, &manifest); err != nil {
		t.Fatalf("decode reload response: %v\nbody:\n%s", err, string(respBody))
	}
	seen := false
	for _, s := range manifest.Skills {
		if s.Name == latecomer && s.Mount == claimedMount {
			seen = true
			if s.State != string(facade.RouteBroken) {
				t.Errorf("reload response: skill %s at mount %s has state %q, want %q (fixture control)", s.Name, s.Mount, s.State, string(facade.RouteBroken))
			}
		}
	}
	if !seen {
		t.Errorf("reload response does not mention skill %s at mount %q, so the mid-session registration was never processed (fixture control)\nbody:\n%s", latecomer, claimedMount, string(respBody))
	}

	// The property: the approved skill keeps serving on its mount after
	// the reload. If the route start installed carried no owner, the
	// facade cannot tell the latecomer's replacement attempt apart from
	// the owning skill's own update, and the mount now answers with the
	// newcomer's not-ready placeholder.
	code, body = getThroughFacade(probeURL, facadeToken)
	if code != http.StatusOK || body != approvedBody {
		t.Errorf("after reload, /%s/answer via the facade is %d %q, want 200 %q: a mid-session registration replaced a live approved skill's route\nstderr:\n%s", claimedMount, code, body, approvedBody, stderrText())
	}

	// Release the inner agent and let runLaunch finish.
	if err := os.WriteFile(exitFile, []byte("done"), 0o644); err != nil {
		t.Fatalf("release inner agent: %v", err)
	}
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("runLaunch did not return after the inner agent exited\nstderr:\n%s", stderrText())
	}
}
