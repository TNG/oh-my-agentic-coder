//go:build darwin && e2e && opencode_v2_repro

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/plugin"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

func TestOpenCodeV2ClaudeMetadataProbe(t *testing.T) {
	openCodeV2ClaudeProbe(t, false, false, "", "")
}

func TestOpenCodeV2ClaudeSkillsReadProbe(t *testing.T) {
	openCodeV2ClaudeProbe(t, true, false, "", "")
}

func TestOpenCodeV2WithoutProjectDiscoveryProbe(t *testing.T) {
	requireOpenCodeV2Host(t)
	t.Setenv("OMAC_BIN", "")
	omac := buildOmac(t)
	for _, mode := range []string{"start", "serve"} {
		t.Run(mode, func(t *testing.T) {
			openCodeV2ClaudeProbe(t, false, false, mode, omac)
		})
	}
}

func TestOpenCodeV2PluginActivationProbe(t *testing.T) {
	openCodeV2ClaudeProbe(t, false, true, "", "")
}

func openCodeV2ClaudeProbe(t *testing.T, readSkills, disableProject bool, productionMode, omac string) {
	t.Helper()
	requireOpenCodeV2Host(t)
	bin := os.Getenv("OC2_BIN")
	if !filepath.IsAbs(bin) {
		t.Fatal("export OC2_BIN with the absolute path to OpenCode 2.0.20")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(version)) != "opencode v2.0.20" {
		t.Fatalf("requires exactly 2.0.20: %v\n%s", err, version)
	}
	hostHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(hostHome, ".claude")
	if productionMode == "" {
		if _, err := os.Lstat(claude); err != nil {
			t.Fatalf("the reported .claude path must exist for this probe: %v", err)
		}
	}
	var extraRead []string
	var skillFile string
	if readSkills {
		skills := filepath.Join(claude, "skills")
		files, err := filepath.Glob(filepath.Join(skills, "*", "SKILL.md"))
		if err != nil || len(files) == 0 {
			t.Fatalf("requires an existing ~/.claude/skills/*/SKILL.md: %v", err)
		}
		extraRead = []string{skills}
		entries, err := os.ReadDir(skills)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			path := filepath.Join(skills, entry.Name())
			resolved, err := filepath.EvalSymlinks(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if resolved != path {
				extraRead = append(extraRead, resolved)
			}
		}
		skillFile = files[0]
	}
	home, workdir := openCodeRestrictedHome(t)
	checkOpenCodeV2Fixture(t, home)
	proveOpenCodeV2FixtureIsolation(t, home, workdir)
	t.Setenv("HOME", home)
	for key, value := range map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
	} {
		t.Setenv(key, value)
	}
	configDir := filepath.Join(home, ".config", "opencode")
	allowed := []string{
		configDir, filepath.Join(home, ".local", "share", "opencode"),
		filepath.Join(home, ".local", "state", "opencode"),
		filepath.Join(home, ".cache", "opencode"),
	}
	for _, dir := range allowed {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if productionMode == "" {
		if _, err := plugin.InstallMultiDirIn(filepath.Join(configDir, "plugins"), false); err != nil {
			t.Fatal(err)
		}
	} else {
		profile := sandboxprofile.DefaultProfile()
		profile.Environment.AllowVars = append(profile.Environment.AllowVars, "OPENCODE_CONFIG_DIR", "OPENCODE_SERVER_PASSWORD")
		data, err := sandboxprofile.MarshalPretty(profile)
		if err != nil {
			t.Fatal(err)
		}
		profileDir := filepath.Join(home, ".config", "omac", "sandbox-profiles")
		if err := os.MkdirAll(profileDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(profileDir, "default.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	var argv []string
	if productionMode == "" {
		g, err := sandboxrun.ResolveGrants(&sandboxprofile.Profile{
			Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
			Filesystem: sandboxprofile.Filesystem{Allow: allowed, Read: extraRead},
			Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeFiltered, OpenPort: []int{port}},
		}, workdir, nil)
		if err != nil {
			t.Fatal(err)
		}
		wrap := func(inner []string) []string {
			t.Helper()
			argv, err := sandboxrun.BuildChildArgv(g, inner)
			if err != nil {
				t.Fatal(err)
			}
			if len(argv) < 3 || argv[1] != "-p" {
				t.Fatal("expected macOS sandbox-exec with an inline profile")
			}
			rule := fmt.Sprintf("(allow file-read-metadata (literal %s))\n", strconv.Quote(claude))
			if readSkills {
				rule += fmt.Sprintf("(allow file-read-metadata file-read-data (literal %s))\n", strconv.Quote(filepath.Join(hostHome, ".opencode")))
			}
			argv[2] = strings.Replace(argv[2], "(deny default)\n", "(deny default)\n"+rule, 1)
			return argv
		}
		type fsProbe struct {
			name string
			args []string
			ok   bool
		}
		probes := []fsProbe{
			{"claude-metadata", []string{"/usr/bin/stat", "-f", "%HT", claude}, true},
			{"claude-listing", []string{"/bin/sh", "-c", `/bin/ls "$1" >/dev/null`, "probe", claude}, readSkills},
			{"outside-file-denied", []string{"/bin/cat", filepath.Join(home, "sibling.txt")}, false},
		}
		if readSkills {
			probes = append(probes, fsProbe{"claude-skill-readable", []string{"/bin/sh", "-c", `/bin/cat "$1" >/dev/null`, "probe", skillFile}, true})
			parentConfig := filepath.Join(hostHome, ".opencode")
			probes = append(probes, fsProbe{"parent-opencode-listing", []string{"/bin/sh", "-c", `/bin/ls "$1" >/dev/null`, "probe", parentConfig}, true})
			configFile := filepath.Join(parentConfig, "opencode.json")
			if _, err := os.Stat(configFile); err == nil {
				probes = append(probes, fsProbe{"parent-opencode-config-denied", []string{"/bin/sh", "-c", `/bin/cat "$1" >/dev/null`, "probe", configFile}, false})
			}
			settings := filepath.Join(claude, "settings.json")
			if _, err := os.Stat(settings); err == nil {
				probes = append(probes, fsProbe{"claude-settings-denied", []string{"/bin/sh", "-c", `/bin/cat "$1" >/dev/null`, "probe", settings}, false})
			}
		}
		for _, probe := range probes {
			argv := wrap(probe.args)
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Dir = workdir
			out, err := cmd.CombinedOutput()
			t.Logf("%s: exit=%v output=%s", probe.name, err, out)
			if ctx.Err() != nil || (err == nil) != probe.ok {
				t.Fatalf("Claude grant premise failed: %s", probe.name)
			}
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() < 0 || !strings.Contains(string(out), "Operation not permitted") {
					t.Fatalf("expected a permission denial: %v\n%s", err, out)
				}
			}
		}
		argv = wrap([]string{bin, "--print-logs", "--log-level", "debug", "serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port)})
	} else {
		argv = []string{omac, productionMode, "--inner", bin, "--open-port", strconv.Itoa(port), "--"}
		if productionMode == "start" {
			argv = append(argv, "serve")
		}
		argv = append(argv, "--hostname", "127.0.0.1", "--port", strconv.Itoa(port))
	}
	logPath := filepath.Join(workdir, "server.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	tmpDir := os.TempDir()
	if productionMode != "" {
		tmpDir, err = os.MkdirTemp("/tmp", "oc2-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(tmpDir); err != nil {
				t.Error(err)
			}
		})
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = workdir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + tmpDir,
		"XDG_CONFIG_HOME=" + os.Getenv("XDG_CONFIG_HOME"), "XDG_DATA_HOME=" + os.Getenv("XDG_DATA_HOME"),
		"XDG_STATE_HOME=" + os.Getenv("XDG_STATE_HOME"), "XDG_CACHE_HOME=" + os.Getenv("XDG_CACHE_HOME"),
		"OPENCODE_CONFIG_DIR=" + configDir, "OPENCODE_SERVER_PASSWORD=omac-metadata-probe",
	}
	if disableProject {
		cmd.Env = append(cmd.Env, "OPENCODE_CONFIG_PROJECT_DISABLE=1")
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.Stdout, cmd.Stderr = log, log
	const shutdownGrace = 10 * time.Second
	cmd.WaitDelay = shutdownGrace
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	sanitize := func(out []byte) []byte {
		if productionMode == "" {
			return out
		}
		out = regexp.MustCompile(`(?i)\b[0-9a-f]{32,}\b`).ReplaceAll(out, []byte("[redacted]"))
		return []byte(strings.ReplaceAll(string(out), "omac-metadata-probe", "[redacted]"))
	}
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	defer func() {
		if err := cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("interrupt foreground server: %v", err)
		}
		select {
		case <-done:
		case <-time.After(shutdownGrace):
			t.Errorf("foreground server shutdown did not complete within %s", shutdownGrace)
			cancel()
		}
		<-done
		if productionMode != "" {
			if ctx.Err() != nil {
				t.Errorf("production %s shutdown context ended: %v; wait error: %v", productionMode, ctx.Err(), waitErr)
			}
			var exit *exec.ExitError
			if waitErr != nil && (!errors.As(waitErr, &exit) || exit.ExitCode() != 130) {
				t.Errorf("production %s foreground server shutdown failed: %v", productionMode, waitErr)
			}
		}
		out, err := os.ReadFile(logPath)
		if err != nil {
			t.Error(err)
		}
		t.Logf("foreground server logs:\n%s", sanitize(out))
	}()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	request := func(path string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth("opencode", "omac-metadata-probe")
		return client.Do(req)
	}
	for {
		select {
		case <-done:
			t.Fatalf("foreground server exited before readiness: %v", waitErr)
		default:
		}
		response, err := request("/api/info")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("server readiness returned HTTP %d", response.StatusCode)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("foreground server did not become ready: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if productionMode != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/info", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("production server password was not enforced: unauthenticated HTTP %d", response.StatusCode)
		}
	}
	client.Timeout = 15 * time.Second
	response, err := request("/api/plugin?" + url.Values{"location[directory]": {workdir}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if productionMode == "" {
		t.Logf("Claude skills read=%t, project discovery disabled=%t: HTTP %d\n%s", readSkills, disableProject, response.StatusCode, body)
	} else {
		t.Logf("production %s project config boot: HTTP %d\n%s", productionMode, response.StatusCode, sanitize(body))
	}
	if response.StatusCode != http.StatusOK {
		if productionMode == "" {
			t.Fatalf("Claude grants did not unblock discovery; see server logs for the next denied operation")
		}
		t.Fatalf("project config boot failed; see server logs for the next denied operation")
	}
	if productionMode != "" {
		if ok, err := plugin.IsMultiDirInstalledIn(filepath.Join(configDir, "plugins")); err != nil || !ok {
			t.Fatalf("production omac plugin auto-install: installed=%t error=%v", ok, err)
		}
		if productionMode == "start" {
			pins, err := filepath.Glob(filepath.Join(tmpDir, "omac-sandbox-tmp-*", "opencode-config", "service.json"))
			if err != nil || len(pins) != 1 {
				t.Fatalf("production start requires one private service pin: count=%d error=%v", len(pins), err)
			}
			data, err := os.ReadFile(pins[0])
			if err != nil {
				t.Fatal(err)
			}
			var pin struct {
				Port int `json:"port"`
			}
			if err := json.Unmarshal(data, &pin); err != nil || pin.Port < 1 || pin.Port > 65535 || pin.Port == port {
				t.Fatalf("invalid private service pin: port=%d error=%v", pin.Port, err)
			}
		}
		t.Logf("production %s: config boot HTTP 200; plugin activation and detached service execution are not asserted", productionMode)
		return
	}
	if !disableProject {
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var inventory struct {
			Data []struct {
				Source struct {
					Path string `json:"path"`
				} `json:"source"`
				State struct {
					Status string `json:"status"`
					Error  string `json:"error"`
				} `json:"state"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &inventory); err != nil {
			t.Fatalf("invalid plugin inventory: %v\n%s", err, body)
		}
		for _, item := range inventory.Data {
			if !strings.HasSuffix(item.Source.Path, "/"+plugin.MultiDirFileName) {
				continue
			}
			t.Logf("omac plugin inventory: %s", body)
			if item.State.Status != "active" {
				t.Fatalf("omac plugin status=%s: %s", item.State.Status, item.State.Error)
			}
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("omac plugin was not observed in inventory; HTTP 200 is not plugin acceptance\n%s", body)
		}
		time.Sleep(100 * time.Millisecond)
		next, err := request("/api/plugin?" + url.Values{"location[directory]": {workdir}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		body, err = io.ReadAll(io.LimitReader(next.Body, 1<<20))
		next.Body.Close()
		if err != nil || next.StatusCode != http.StatusOK {
			t.Fatalf("plugin inventory polling: HTTP %d error=%v\n%s", next.StatusCode, err, body)
		}
	}
}
