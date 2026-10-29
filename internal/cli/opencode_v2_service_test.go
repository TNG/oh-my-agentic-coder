package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// fakeVersionBinary writes an executable sh script that prints script as its --version output.
func fakeVersionBinary(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a sh script")
	}
	bin := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestOpenCodeIsV2(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   bool
	}{
		{"v2 plain", "#!/bin/sh\necho 2.0.18\n", true},
		{"v2 tagged", "#!/bin/sh\necho v2.1.0\n", true},
		{"v1 plain", "#!/bin/sh\necho 1.18.32\n", false},
		{"v1 npm triple", "#!/bin/sh\necho opencode-cli/1.17.12 linux-x64 node-v20.0.0\n", false},
		{"failing --version treated as current", "#!/bin/sh\nexit 1\n", true},
		{"empty output treated as current", "#!/bin/sh\nprintf ''\n", true},
		{"no semver treated as current", "#!/bin/sh\necho dev\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeVersionBinary(t, tc.script)
			if got := openCodeIsV2([]string{bin}); got != tc.want {
				t.Errorf("openCodeIsV2(%s) = %v, want %v", tc.script, got, tc.want)
			}
		})
	}
}

// An unresolvable inner is checkInnerBinary's job; detection must not panic or treat it as v2.
func TestOpenCodeIsV2SkipsMissingBinary(t *testing.T) {
	if openCodeIsV2([]string{"omac-definitely-not-on-path-xyz"}) {
		t.Error("missing binary must not be treated as v2")
	}
	if openCodeIsV2(nil) || openCodeIsV2([]string{}) {
		t.Error("empty inner must not be treated as v2")
	}
}

// Wrapper runtimes resolve the real binary only at runtime, so a versioned spec in their args decides — no exec.
func TestOpenCodeIsV2FromWrapperSpec(t *testing.T) {
	cases := []struct {
		name  string
		inner []string
		want  bool
	}{
		{"bunx v2 spec", []string{"bunx", "opencode-ai@2.0.1"}, true},
		{"npx with flags v1 spec", []string{"npx", "-y", "opencode-ai@1.2.3"}, false},
		{"pnpm dlx v1 spec", []string{"pnpm", "dlx", "opencode@1.9.0", "run"}, false},
		{"env-wrapped npx v2", []string{"env", "FOO=1", "npx", "opencode-ai@2.0.18"}, true},
		{"v tag", []string{"bunx", "opencode-ai@v2.1.0"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := openCodeIsV2(tc.inner); got != tc.want {
				t.Errorf("openCodeIsV2(%v) = %v, want %v", tc.inner, got, tc.want)
			}
		})
	}
}

func TestOpenCodeSpecVersion(t *testing.T) {
	maj, min, hasMinor, ok := openCodeSpecVersion([]string{"-y", "--package=opencode@1.3.17", "opencode"})
	if !ok || maj != 1 || min != 3 || !hasMinor {
		t.Errorf("openCodeSpecVersion = (%d, %d, %v, %v), want (1, 3, true, true)", maj, min, hasMinor, ok)
	}
	maj, min, hasMinor, ok = openCodeSpecVersion([]string{"bunx", "opencode-ai@2", "run"})
	if !ok || maj != 2 || hasMinor || min != 0 {
		t.Errorf("openCodeSpecVersion major-only = (%d, %d, %v, %v), want (2, 0, false, true)", maj, min, hasMinor, ok)
	}
	maj, min, hasMinor, ok = openCodeSpecVersion([]string{"npx", "-y", "@opencode/cli@2.1-rc"})
	if !ok || maj != 2 || min != 1 || !hasMinor {
		t.Errorf("openCodeSpecVersion pre-release = (%d, %d, %v, %v), want (2, 1, true, true)", maj, min, hasMinor, ok)
	}
	if _, _, _, ok := openCodeSpecVersion([]string{"opencode-ai@latest"}); ok {
		t.Error("spec without a pinned major version must be undecidable")
	}
}

// Known wrapper without a spec: undecidable → pin, no version probe may run.
func TestOpenCodeIsV2UndecidableWrapperPins(t *testing.T) {
	if !openCodeIsV2([]string{"bunx", "opencode-ai"}) {
		t.Error("undecidable known wrapper must default to pin")
	}
}

// A profile denying the vars the pin redirects must stop the launch instead of silently stripping the pin.
func TestOpenCodePinDenied(t *testing.T) {
	denies := func(vars ...string) *sandboxprofile.Profile {
		return &sandboxprofile.Profile{Environment: sandboxprofile.Environment{DenyVars: vars}}
	}
	cases := []struct {
		name    string
		plan    sandboxPlan
		blocked bool
	}{
		{"no policy", sandboxPlan{Native: true}, false},
		{"clean profile", sandboxPlan{PolicyRef: "default", Policy: &sandboxprofile.Profile{}}, false},
		{"exact XDG_STATE_HOME", sandboxPlan{PolicyRef: "default", Policy: denies("HTTP_PROXY", "XDG_STATE_HOME")}, true},
		{"exact OPENCODE_CONFIG_DIR", sandboxPlan{PolicyRef: "default", Policy: denies("OPENCODE_CONFIG_DIR")}, true},
		{"XDG_* prefix", sandboxPlan{PolicyRef: "default", Policy: denies("XDG_*")}, true},
		{"deny star", sandboxPlan{PolicyRef: "default", Policy: denies("*")}, true},
		{"allow-only profile", sandboxPlan{PolicyRef: "default", Policy: &sandboxprofile.Profile{Environment: sandboxprofile.Environment{AllowVars: []string{"PATH"}}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := openCodePinDenied(tc.plan)
			if tc.blocked && err == nil {
				t.Error("expected denial error, got nil")
			}
			if !tc.blocked && err != nil {
				t.Errorf("unexpected denial: %v", err)
			}
		})
	}

	err := openCodePinDenied(sandboxPlan{PolicyRef: "default", Policy: denies("XDG_STATE_HOME")})
	if err == nil || !strings.Contains(err.Error(), "XDG_STATE_HOME") || !strings.Contains(err.Error(), `"default"`) {
		t.Errorf("denial error must name the var and the profile: %v", err)
	}
}

// Core of the fix: a v2 inner gets a free port, a symlink-seeded config dir, and a private state dir.
func TestPinOpenCodeV2ServiceSeedsConfig(t *testing.T) {
	h, ok := config.LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}

	// ConfigHome() resolves $XDG_CONFIG_HOME/opencode, so the test owns it.
	cfgRoot := t.TempDir()
	realCfg := filepath.Join(cfgRoot, "opencode")
	if err := os.MkdirAll(filepath.Join(realCfg, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realCfg, "opencode.json"), []byte(`{"model":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realCfg, "service.json"), []byte(`{"port":49374,"password":"hostpw"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfgRoot)
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	hostModel := `{"favorite":[{"providerID":"trustedtokens","modelID":"deepseek"}]}`
	writeTestFile(t, filepath.Join(stateHome, "opencode", openCodeModelStateFile), hostModel)

	sandboxTmp := t.TempDir()
	inner := []string{fakeVersionBinary(t, "#!/bin/sh\necho 2.0.18\n")}

	pin, err := pinOpenCodeV2Service(h, inner, sandboxTmp)
	if err != nil {
		t.Fatalf("pinOpenCodeV2Service: %v", err)
	}
	if pin == nil {
		t.Fatal("v2 inner command produced no pin")
	}
	defer pin.release()
	if pin.Port == openCodeDefaultServicePort {
		t.Errorf("pin picked OpenCode's default shared-service port %d", pin.Port)
	}
	if pin.Port < 1 || pin.Port > 65535 {
		t.Errorf("port out of range: %d", pin.Port)
	}
	if want := filepath.Join(sandboxTmp, "opencode-config"); pin.CfgDir != want {
		t.Errorf("CfgDir = %q, want %q", pin.CfgDir, want)
	}
	if want := filepath.Join(sandboxTmp, "opencode-state"); pin.StateDir != want {
		t.Errorf("StateDir = %q, want %q", pin.StateDir, want)
	}
	// The pin itself seeds the host model choice; deleting that call would
	// pass the direct seed/save tests and silently forget the choice.
	if got, err := os.ReadFile(filepath.Join(pin.StateDir, "opencode", openCodeModelStateFile)); err != nil || string(got) != hostModel {
		t.Errorf("pin did not seed the host model state: %q, %v", got, err)
	}

	// Only the pinned port: the host's password and port must not leak into the sandboxed service identity.
	raw, err := os.ReadFile(filepath.Join(pin.CfgDir, "service.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("service.json %q: %v", raw, err)
	}
	if len(cfg) != 1 {
		t.Errorf("service.json = %v, want exactly the port field", cfg)
	}
	if port, _ := cfg["port"].(float64); int(port) != pin.Port {
		t.Errorf("service.json port = %v, want %d", cfg["port"], pin.Port)
	}
	if st, err := os.Lstat(filepath.Join(pin.CfgDir, "service.json")); err != nil {
		t.Fatal(err)
	} else if st.Mode()&os.ModeSymlink != 0 {
		t.Error("service.json must be the private pin, not a symlink to the host config")
	}

	// User config entries come through as symlinks to the real config home.
	if st, err := os.Lstat(filepath.Join(pin.CfgDir, "opencode.json")); err != nil {
		t.Fatal(err)
	} else if st.Mode()&os.ModeSymlink == 0 {
		t.Error("opencode.json should be a symlink into the real config home")
	}
	if got, err := os.ReadFile(filepath.Join(pin.CfgDir, "opencode.json")); err != nil {
		t.Errorf("reading through symlink: %v", err)
	} else if string(got) != `{"model":"x"}` {
		t.Errorf("symlinked opencode.json = %q", got)
	}
	if st, err := os.Lstat(filepath.Join(pin.CfgDir, "plugins")); err != nil {
		t.Fatal(err)
	} else if st.Mode()&os.ModeSymlink == 0 {
		t.Error("plugins/ should be a symlink into the real config home")
	}

	// The host's own service config stays untouched.
	if host, err := os.ReadFile(filepath.Join(realCfg, "service.json")); err != nil {
		t.Fatal(err)
	} else if string(host) != `{"port":49374,"password":"hostpw"}` {
		t.Errorf("host service.json modified: %s", host)
	}
}

// v1 (the pinned e2e suite) keeps the unchanged path: no pin, no dirs, no extra grants.
func TestPinOpenCodeV2ServiceSkipsV1(t *testing.T) {
	h, _ := config.LookupHarness("opencode")
	sandboxTmp := t.TempDir()
	inner := []string{fakeVersionBinary(t, "#!/bin/sh\necho 1.17.12\n")}

	pin, err := pinOpenCodeV2Service(h, inner, sandboxTmp)
	if err != nil {
		t.Fatalf("pinOpenCodeV2Service: %v", err)
	}
	if pin != nil {
		t.Fatalf("v1 must not be pinned, got %+v", pin)
	}
	if _, err := os.Stat(filepath.Join(sandboxTmp, "opencode-config")); !os.IsNotExist(err) {
		t.Error("v1 launch must not seed a config dir")
	}
}

// The pin is opencode-specific; another harness (even with a v2-looking binary) gets nothing.
func TestPinOpenCodeV2ServiceSkipsOtherHarness(t *testing.T) {
	cc, ok := config.LookupHarness("claude-code")
	if !ok {
		t.Fatal("claude-code harness not found")
	}
	sandboxTmp := t.TempDir()
	inner := []string{fakeVersionBinary(t, "#!/bin/sh\necho 2.0.18\n")}

	pin, err := pinOpenCodeV2Service(cc, inner, sandboxTmp)
	if err != nil {
		t.Fatalf("pinOpenCodeV2Service: %v", err)
	}
	if pin != nil {
		t.Fatalf("non-opencode harness must not be pinned, got %+v", pin)
	}
	if _, err := os.Stat(filepath.Join(sandboxTmp, "opencode-config")); !os.IsNotExist(err) {
		t.Error("non-opencode launch must not seed a config dir")
	}
}

// install splices both flags on native plans (allow-env is load-bearing: FilterEnv strips the env otherwise) and nothing on non-native.
func TestOpenCodeServicePinInstall(t *testing.T) {
	pin := &openCodeServicePin{Port: 59991, CfgDir: "/tmp/t/opencode-config", StateDir: "/tmp/t/opencode-state"}
	in := []string{"omac", "sandbox", "run", "--profile", "default", "--", "opencode"}

	native := sandboxPlan{Native: true, PolicyRef: "default"}
	want := []string{"omac", "sandbox", "run", "--profile", "default",
		"--open-port", "59991", "--allow-env", "OPENCODE_CONFIG_DIR", "--", "opencode"}
	if got := pin.install(in, native); !equalStrings(got, want) {
		t.Errorf("install(native): got %v, want %v", got, want)
	}

	if got := pin.install(in, sandboxPlan{Native: false}); !equalStrings(got, in) {
		t.Errorf("install(non-native) must not splice flags: got %v, want %v", got, in)
	}
}

// apply lands both env keys in the extra map without clobbering unrelated keys.
func TestOpenCodeServicePinApply(t *testing.T) {
	pin := &openCodeServicePin{Port: 59991, CfgDir: "/tmp/t/opencode-config", StateDir: "/tmp/t/opencode-state"}

	extra := map[string]string{"TMPDIR": "/tmp/t"}
	pin.apply(extra)
	if extra["OPENCODE_CONFIG_DIR"] != pin.CfgDir {
		t.Errorf("OPENCODE_CONFIG_DIR = %q, want %q", extra["OPENCODE_CONFIG_DIR"], pin.CfgDir)
	}
	if extra["XDG_STATE_HOME"] != pin.StateDir {
		t.Errorf("XDG_STATE_HOME = %q, want %q", extra["XDG_STATE_HOME"], pin.StateDir)
	}
	if extra["TMPDIR"] != "/tmp/t" {
		t.Errorf("apply must not clobber unrelated keys, TMPDIR = %q", extra["TMPDIR"])
	}
}

// holdPort binds a free loopback port for the test and returns it.
func holdPort(t *testing.T) (int, net.Listener) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().(*net.TCPAddr).Port, l
}

// freePort returns a loopback port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	port, l := holdPort(t)
	_ = l.Close()
	return port
}

func writeJSONFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The host service's ports are never picked: the default, the configured port, and the registered one.
func TestHostOpenCodeServicePorts(t *testing.T) {
	h, _ := config.LookupHarness("opencode")
	cfgRoot, stateRoot, extraCfg := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgRoot)
	t.Setenv("XDG_STATE_HOME", stateRoot)
	t.Setenv("OPENCODE_CONFIG_DIR", "")

	ports := hostOpenCodeServicePorts(h)
	if len(ports) != 1 || ports[openCodeDefaultServicePort] == "" {
		t.Fatalf("without host files only the default port is excluded, got %v", ports)
	}

	writeJSONFile(t, filepath.Join(cfgRoot, "opencode", "service.json"), `{"port":52001,"password":"x"}`)
	writeJSONFile(t, filepath.Join(extraCfg, "service.json"), `{"port":52002}`)
	writeJSONFile(t, filepath.Join(stateRoot, "opencode", "service.json"), `{"url":"http://127.0.0.1:52003","pid":4242,"password":"x"}`)
	t.Setenv("OPENCODE_CONFIG_DIR", extraCfg)

	ports = hostOpenCodeServicePorts(h)
	for _, p := range []int{openCodeDefaultServicePort, 52001, 52002, 52003} {
		if ports[p] == "" {
			t.Errorf("port %d not excluded: %v", p, ports)
		}
	}
	if !strings.Contains(ports[52003], "pid 4242") {
		t.Errorf("registered port reason should name the host service pid: %q", ports[52003])
	}

	// Garbage files are ignored, never fatal.
	writeJSONFile(t, filepath.Join(cfgRoot, "opencode", "service.json"), `not json`)
	if ports := hostOpenCodeServicePorts(h); ports[52001] != "" {
		t.Errorf("unparsable config must not add a port: %v", ports)
	}
}

func TestOpenCodeServiceInfoPort(t *testing.T) {
	cases := []struct {
		info openCodeServiceInfo
		want int
	}{
		{openCodeServiceInfo{Port: 5000}, 5000},
		{openCodeServiceInfo{URL: "http://127.0.0.1:5001"}, 5001},
		{openCodeServiceInfo{Port: 5002, URL: "http://127.0.0.1:5003"}, 5002},
		{openCodeServiceInfo{URL: "http://127.0.0.1"}, 0},
		{openCodeServiceInfo{URL: "::bad"}, 0},
		{openCodeServiceInfo{}, 0},
	}
	for _, tc := range cases {
		if got := tc.info.port(); got != tc.want {
			t.Errorf("%+v.port() = %d, want %d", tc.info, got, tc.want)
		}
	}
}

// A kernel pick that lands on a host service port is skipped (and not handed out again); the next one is used and held.
func TestReserveServicePortSkipsHostPorts(t *testing.T) {
	t.Setenv(openCodeServicePortEnv, "")
	excluded, _ := holdPort(t)
	l1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	picks := []net.Listener{l1, l2}
	host := map[int]string{l1.Addr().(*net.TCPAddr).Port: "is a host port", excluded: "is a host port"}
	orig := listenAnyLoopback
	listenAnyLoopback = func() (net.Listener, error) {
		l := picks[0]
		picks = picks[1:]
		return l, nil
	}
	t.Cleanup(func() { listenAnyLoopback = orig })

	port, hold, err := reserveServicePort(host)
	if err != nil {
		t.Fatalf("reserveServicePort: %v", err)
	}
	defer hold.Close()
	if want := l2.Addr().(*net.TCPAddr).Port; port != want {
		t.Errorf("port = %d, want the second, non-excluded pick %d", port, want)
	}
	if _, err := net.Listen("tcp", loopbackAddr(port)); err == nil {
		t.Error("the reserved port must stay bound until handOff")
	}
	// The skipped pick is released once the reservation is made.
	if l, err := net.Listen("tcp", l1.Addr().String()); err != nil {
		t.Errorf("skipped pick still bound: %v", err)
	} else {
		_ = l.Close()
	}
}

// Every pick excluded → a clear error instead of an endless loop or a host port.
func TestReserveServicePortAllExcluded(t *testing.T) {
	t.Setenv(openCodeServicePortEnv, "")
	host := map[int]string{}
	orig := listenAnyLoopback
	listenAnyLoopback = func() (net.Listener, error) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			host[l.Addr().(*net.TCPAddr).Port] = "is a host port"
		}
		return l, err
	}
	t.Cleanup(func() { listenAnyLoopback = orig })
	if _, _, err := reserveServicePort(host); err == nil || !strings.Contains(err.Error(), "no free loopback port") {
		t.Errorf("want no-free-port error, got %v", err)
	}
}

// The test hook forces the port but never bypasses the checks.
func TestReserveServicePortForced(t *testing.T) {
	t.Run("taken", func(t *testing.T) {
		port, _ := holdPort(t)
		t.Setenv(openCodeServicePortEnv, strconv.Itoa(port))
		_, _, err := reserveServicePort(nil)
		var pe *servicePortError
		if !errors.As(err, &pe) || pe.Port != port {
			t.Fatalf("want servicePortError for port %d, got %v", port, err)
		}
		for _, want := range []string{strconv.Itoa(port), "already in use by another process", "run the command again"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err, want)
			}
		}
	})
	t.Run("host port", func(t *testing.T) {
		t.Setenv(openCodeServicePortEnv, strconv.Itoa(openCodeDefaultServicePort))
		_, _, err := reserveServicePort(map[int]string{openCodeDefaultServicePort: "is OpenCode's default shared-service port"})
		if err == nil || !strings.Contains(err.Error(), "default shared-service port") {
			t.Errorf("want default-port refusal, got %v", err)
		}
	})
	t.Run("free", func(t *testing.T) {
		want := freePort(t)
		t.Setenv(openCodeServicePortEnv, strconv.Itoa(want))
		port, hold, err := reserveServicePort(nil)
		if err != nil {
			t.Fatalf("reserveServicePort: %v", err)
		}
		defer hold.Close()
		if port != want {
			t.Errorf("port = %d, want forced %d", port, want)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		t.Setenv(openCodeServicePortEnv, "70000")
		if _, _, err := reserveServicePort(nil); err == nil {
			t.Error("out-of-range forced port must be rejected")
		}
	})
}

func TestCheckServicePortFree(t *testing.T) {
	if err := checkServicePortFree(freePort(t), nil); err != nil {
		t.Errorf("free port refused: %v", err)
	}
	taken, _ := holdPort(t)
	if err := checkServicePortFree(taken, nil); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Errorf("taken port: want in-use error, got %v", err)
	}
	free := freePort(t)
	err := checkServicePortFree(free, map[int]string{free: "is registered by the host OpenCode shared service (pid 7)"})
	if err == nil || !strings.Contains(err.Error(), "host OpenCode shared service") {
		t.Errorf("host-registered port: want refusal, got %v", err)
	}
}

// handOff frees the held port and re-checks it, including a host service that registered the port meanwhile.
func TestOpenCodeServicePinHandOff(t *testing.T) {
	port, hold := holdPort(t)
	pin := &openCodeServicePin{Port: port, hold: hold}
	if err := pin.handOff(); err != nil {
		t.Fatalf("handOff of a held, free port: %v", err)
	}
	if l, err := net.Listen("tcp", loopbackAddr(port)); err != nil {
		t.Errorf("port still bound after handOff: %v", err)
	} else {
		_ = l.Close()
	}
	pin.release() // idempotent
	var nilPin *openCodeServicePin
	nilPin.release()

	port, hold = holdPort(t)
	pin = &openCodeServicePin{Port: port, hold: hold, hostPorts: func() map[int]string {
		return map[int]string{port: "is registered by the host OpenCode shared service"}
	}}
	if err := pin.handOff(); err == nil {
		t.Error("handOff must refuse a port the host service registered meanwhile")
	}
}

func TestParseLsofHolder(t *testing.T) {
	cases := map[string]string{
		"p41394\ncPython\n":          "Python, pid 41394",
		"p1\ncfirst\np2\ncsecond\n":  "first, pid 1",
		"p77\n":                      "pid 77",
		"":                           "",
		"garbage\n":                  "",
		"p5\nccmd with spaces\nf7\n": "cmd with spaces, pid 5",
	}
	for in, want := range cases {
		if got := parseLsofHolder(in); got != want {
			t.Errorf("parseLsofHolder(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeOpenCodeService serves /api/info like OpenCode v2: basic auth opencode:<password>, then {"pid":pid}.
func fakeOpenCodeService(t *testing.T, password string, pid int) int {
	t.Helper()
	port, l := holdPort(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pw, ok := r.BasicAuth()
		if !ok || user != "opencode" || pw != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"version":"2.0.26","pid":%d}`, pid)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return port
}

func writeRegistration(t *testing.T, stateDir string, port, pid int, password string) {
	t.Helper()
	writeJSONFile(t, filepath.Join(stateDir, "opencode", "service.json"),
		fmt.Sprintf(`{"id":"x","version":"2.0.26","url":"http://127.0.0.1:%d","pid":%d,"password":%q}`, port, pid, password))
}

var fastServiceCheck = serviceCheck{Poll: 20 * time.Millisecond, Grace: 300 * time.Millisecond, Window: 500 * time.Millisecond}

func TestVerifyService(t *testing.T) {
	t.Run("own service proven", func(t *testing.T) {
		port := fakeOpenCodeService(t, "pw", 4321)
		pin := &openCodeServicePin{Port: port, StateDir: t.TempDir()}
		writeRegistration(t, pin.StateDir, port, 4321, "pw")
		pid, err := pin.verifyService(context.Background(), fastServiceCheck)
		if err != nil || pid != 4321 {
			t.Errorf("verifyService = (%d, %v), want (4321, nil)", pid, err)
		}
	})
	t.Run("foreign listener without registration", func(t *testing.T) {
		port, _ := holdPort(t)
		pin := &openCodeServicePin{Port: port, StateDir: t.TempDir()}
		start := time.Now()
		_, err := pin.verifyService(context.Background(), fastServiceCheck)
		if err == nil || !strings.Contains(err.Error(), "taken by another process") {
			t.Fatalf("want taken-port error, got %v", err)
		}
		if waited := time.Since(start); waited < fastServiceCheck.Grace {
			t.Errorf("refused after %v, before the %v grace", waited, fastServiceCheck.Grace)
		}
	})
	t.Run("listener rejects the registration", func(t *testing.T) {
		// A different service (other credentials) holds the port while our registration claims it.
		port := fakeOpenCodeService(t, "someone-else", 99)
		pin := &openCodeServicePin{Port: port, StateDir: t.TempDir()}
		writeRegistration(t, pin.StateDir, port, 4321, "pw")
		if _, err := pin.verifyService(context.Background(), fastServiceCheck); err == nil {
			t.Error("a listener that does not answer for the registration must be refused")
		}
	})
	t.Run("pid mismatch", func(t *testing.T) {
		port := fakeOpenCodeService(t, "pw", 99)
		pin := &openCodeServicePin{Port: port, StateDir: t.TempDir()}
		writeRegistration(t, pin.StateDir, port, 4321, "pw")
		if _, err := pin.verifyService(context.Background(), fastServiceCheck); err == nil {
			t.Error("a service reporting another pid must be refused")
		}
	})
	t.Run("registered another port", func(t *testing.T) {
		pin := &openCodeServicePin{Port: freePort(t), StateDir: t.TempDir()}
		writeRegistration(t, pin.StateDir, pin.Port+1, 4321, "pw")
		if _, err := pin.verifyService(context.Background(), fastServiceCheck); err == nil || !strings.Contains(err.Error(), "registered port") {
			t.Errorf("want registered-port error, got %v", err)
		}
	})
	t.Run("service never started", func(t *testing.T) {
		pin := &openCodeServicePin{Port: freePort(t), StateDir: t.TempDir()}
		pid, err := pin.verifyService(context.Background(), fastServiceCheck)
		if err != nil || pid != 0 {
			t.Errorf("verifyService = (%d, %v), want (0, nil) when nothing listens", pid, err)
		}
	})
	t.Run("session ended", func(t *testing.T) {
		port, _ := holdPort(t)
		pin := &openCodeServicePin{Port: port, StateDir: t.TempDir()}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := pin.verifyService(ctx, serviceCheck{Poll: time.Second, Grace: time.Hour, Window: time.Hour}); err != nil {
			t.Errorf("a finished session must not be refused afterwards: %v", err)
		}
	})
}

// writeTestFile creates path and its parents without the production writer,
// so a path bug in writeFileAtomic cannot hide behind matching fixtures.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The model choice survives into the next launch, while the host service
// registration stays out of the sandbox in both directions.
func TestOpenCodeModelStatePersistsAcrossPins(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	host := filepath.Join(stateHome, "opencode", openCodeModelStateFile)
	writeTestFile(t, host, `{"variant":{"trustedtokens/deepseek":"high"}}`)
	writeTestFile(t, filepath.Join(stateHome, "opencode", "service.json"), `{"port":49374}`)

	first := &openCodeServicePin{StateDir: filepath.Join(t.TempDir(), "opencode-state")}
	if err := first.seedModelState(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(first.StateDir, "opencode", "service.json")); !os.IsNotExist(err) {
		t.Fatalf("host service.json must not be seeded into the sandbox: %v", err)
	}
	if got, err := os.ReadFile(first.sessionModelStatePath()); err != nil || string(got) != `{"variant":{"trustedtokens/deepseek":"high"}}` {
		t.Fatalf("seeded model state = %q, %v", got, err)
	}

	writeTestFile(t, first.sessionModelStatePath(), `{"variant":{"trustedtokens/deepseek":"max"}}`)
	writeTestFile(t, filepath.Join(first.StateDir, "opencode", "service.json"), `{"port":12345}`)
	if err := first.saveModelState(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got, err := os.ReadFile(host); err != nil || string(got) != `{"variant":{"trustedtokens/deepseek":"max"}}` {
		t.Fatalf("saved model state = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(stateHome, "opencode", "service.json")); err != nil || string(got) != `{"port":49374}` {
		t.Fatalf("host service.json must stay untouched: %q, %v", got, err)
	}
}

// Most users do not set XDG_STATE_HOME, so the ~/.local/state fallback is the
// path that matters in production; it needs its own round-trip test.
func TestOpenCodeModelStateDefaultStateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")

	pin := &openCodeServicePin{StateDir: filepath.Join(t.TempDir(), "opencode-state")}
	host := filepath.Join(home, ".local", "state", "opencode", openCodeModelStateFile)
	if got := pin.hostModelStatePath(); got != host {
		t.Fatalf("hostModelStatePath = %q, want %q", got, host)
	}

	// A first launch has no host file; seed must be a no-op, not an error.
	if err := pin.seedModelState(); err != nil {
		t.Fatalf("seed without a host file: %v", err)
	}
	if _, err := os.Stat(pin.sessionModelStatePath()); !os.IsNotExist(err) {
		t.Fatalf("seed must not create a session file: %v", err)
	}

	writeTestFile(t, host, `{"variant":{"trustedtokens/deepseek":"high"}}`)
	if err := pin.seedModelState(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got, err := os.ReadFile(pin.sessionModelStatePath()); err != nil || string(got) != `{"variant":{"trustedtokens/deepseek":"high"}}` {
		t.Fatalf("seeded model state = %q, %v", got, err)
	}

	writeTestFile(t, pin.sessionModelStatePath(), `{"variant":{"trustedtokens/deepseek":"max"}}`)
	if err := pin.saveModelState(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got, err := os.ReadFile(host); err != nil || string(got) != `{"variant":{"trustedtokens/deepseek":"max"}}` {
		t.Fatalf("saved model state = %q, %v", got, err)
	}
	if st, err := os.Stat(filepath.Dir(host)); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm() != 0o700 {
		t.Errorf("host state dir mode = %o, want 0700", st.Mode().Perm())
	}
	if st, err := os.Stat(host); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("host model.json mode = %o, want 0600", st.Mode().Perm())
	}
}

// A session that never wrote a model.json must not touch the host copy. This
// is the os.IsNotExist branch, and the dangerous one to regress.
func TestOpenCodeModelStateSaveWithoutSessionFile(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	host := filepath.Join(stateHome, "opencode", openCodeModelStateFile)
	writeTestFile(t, host, `{"variant":{"trustedtokens/deepseek":"high"}}`)

	pin := &openCodeServicePin{StateDir: filepath.Join(t.TempDir(), "opencode-state")}
	if err := pin.saveModelState(); err != nil {
		t.Fatalf("save without a session file: %v", err)
	}
	if got, err := os.ReadFile(host); err != nil || string(got) != `{"variant":{"trustedtokens/deepseek":"high"}}` {
		t.Fatalf("host model state = %q, %v, want it untouched", got, err)
	}
	if _, err := os.Stat(pin.StateDir); !os.IsNotExist(err) {
		t.Errorf("save must not create the session state dir: %v", err)
	}
}

// No-op sessions and unreadable session data must leave the host file alone:
// writing back identical bytes is pointless, and empty/truncated bytes would
// destroy the very preference this path exists to keep.
func TestOpenCodeModelStateSaveSkipsUnchangedAndInvalid(t *testing.T) {
	const hostBody = `{"variant":{"trustedtokens/deepseek":"high"}}`
	cases := []struct {
		name    string
		session string
		wantErr bool
	}{
		{"unchanged", hostBody, false},
		{"empty", "", true},
		{"truncated", `{"variant":`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateHome := t.TempDir()
			t.Setenv("XDG_STATE_HOME", stateHome)
			host := filepath.Join(stateHome, "opencode", openCodeModelStateFile)
			writeTestFile(t, host, hostBody)
			old := time.Unix(1000, 0)
			if err := os.Chtimes(host, old, old); err != nil {
				t.Fatal(err)
			}

			pin := &openCodeServicePin{StateDir: filepath.Join(t.TempDir(), "opencode-state")}
			writeTestFile(t, pin.sessionModelStatePath(), tc.session)
			err := pin.saveModelState()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
					t.Fatalf("save = %v, want a not-valid-JSON error", err)
				}
			} else if err != nil {
				t.Fatalf("save: %v", err)
			}
			if got, err := os.ReadFile(host); err != nil || string(got) != hostBody {
				t.Fatalf("host model state = %q, %v, want it untouched", got, err)
			}
			if st, err := os.Stat(host); err != nil {
				t.Fatal(err)
			} else if !st.ModTime().Equal(old) {
				t.Errorf("host model state was rewritten (mtime %v), want %v", st.ModTime(), old)
			}
		})
	}
}

// A symlinked host model.json (dotfiles repo) must keep its link; the save
// goes to the link target instead of replacing the link with a regular file.
func TestOpenCodeModelStateSaveThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	target := filepath.Join(t.TempDir(), "dotfiles-model.json")
	writeTestFile(t, target, `{"variant":{"trustedtokens/deepseek":"high"}}`)
	link := filepath.Join(stateHome, "opencode", openCodeModelStateFile)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	pin := &openCodeServicePin{StateDir: filepath.Join(t.TempDir(), "opencode-state")}
	writeTestFile(t, pin.sessionModelStatePath(), `{"variant":{"trustedtokens/deepseek":"max"}}`)
	if err := pin.saveModelState(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if st, err := os.Lstat(link); err != nil {
		t.Fatal(err)
	} else if st.Mode()&os.ModeSymlink == 0 {
		t.Error("save replaced the symlink with a regular file")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != `{"variant":{"trustedtokens/deepseek":"max"}}` {
		t.Fatalf("symlink target = %q, %v", got, err)
	}
}
