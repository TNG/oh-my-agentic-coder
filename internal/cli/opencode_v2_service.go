package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

// openCodeServicePin pins opencode v2's shared background service to a free per-launch loopback port in the sandbox.
type openCodeServicePin struct {
	Port     int
	CfgDir   string
	StateDir string
}

// grant splices the pinned port into the sandbox argv as --open-port.
func (p *openCodeServicePin) grant(argv []string) []string {
	return injectOpenPort(argv, strconv.Itoa(p.Port))
}

// apply exports the pin into the sandbox runtime's environment.
func (p *openCodeServicePin) apply(extra map[string]string) {
	extra["OPENCODE_CONFIG_DIR"] = p.CfgDir
	extra["XDG_STATE_HOME"] = p.StateDir
}

// install splices the port grant and OPENCODE_CONFIG_DIR's --allow-env into argv; native backend only.
func (p *openCodeServicePin) install(argv []string, plan sandboxPlan) []string {
	if !plan.Native {
		return argv
	}
	argv = p.grant(argv)
	// FilterEnv's allowlist strips OPENCODE_CONFIG_DIR without this.
	return injectSandboxEnvAllow(argv, []string{"OPENCODE_CONFIG_DIR"}, plan)
}

// pinOpenCodeV2Service returns the pin for opencode v2+ inner commands, or nil for v1/other harnesses.
func pinOpenCodeV2Service(h config.Harness, inner []string, sandboxTmp string) (*openCodeServicePin, error) {
	if h.Name != "opencode" || sandboxTmp == "" {
		return nil, nil
	}
	if !openCodeIsV2(inner) {
		return nil, nil
	}
	port, err := freeLoopbackPort()
	if err != nil {
		return nil, fmt.Errorf("pick service port: %w", err)
	}
	cfgDir, err := seedOpenCodeServiceConfig(h.ConfigHome(), filepath.Join(sandboxTmp, "opencode-config"), port)
	if err != nil {
		return nil, err
	}
	return &openCodeServicePin{
		Port:     port,
		CfgDir:   cfgDir,
		StateDir: filepath.Join(sandboxTmp, "opencode-state"),
	}, nil
}

// freeLoopbackPort returns a loopback port free right now; probe-then-bind is a known TOCTOU race.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

// seedOpenCodeServiceConfig symlinks realCfg into cfgDir and replaces service.json with {"port":P} (no host password).
func seedOpenCodeServiceConfig(realCfg, cfgDir string, port int) (string, error) {
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", cfgDir, err)
	}
	if realCfg != "" {
		entries, err := os.ReadDir(realCfg)
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("read %s: %w", realCfg, err)
		}
		for _, e := range entries {
			if e.Name() == "service.json" {
				continue
			}
			link := filepath.Join(cfgDir, e.Name())
			if err := os.Symlink(filepath.Join(realCfg, e.Name()), link); err != nil {
				return "", fmt.Errorf("link %s: %w", e.Name(), err)
			}
		}
	}
	cfg, err := json.Marshal(struct {
		Port int `json:"port"`
	}{Port: port})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "service.json"), cfg, 0o600); err != nil {
		return "", fmt.Errorf("write service config: %w", err)
	}
	return cfgDir, nil
}

// openCodeVersionRe matches the first semver-ish token in --version output.
var openCodeVersionRe = regexp.MustCompile(`v?(\d+)\.\d+\.\d+`)

// openCodeWrappers are package runners whose cmd[0] is the runner itself, not the opencode binary.
var openCodeWrappers = map[string]bool{
	"npx": true, "bunx": true, "pnpm": true, "yarn": true, "uvx": true, "uv": true,
}

// openCodeSpecRe detects a versioned opencode package spec in wrapper args (opencode-ai@2.1.0, --package=opencode@1.3.17).
var openCodeSpecRe = regexp.MustCompile(`opencode(?:-ai)?@v?(\d+)`)

// openCodeIsV2 reports whether inner is opencode v2+; wrappers decide from a versioned package spec in their args, only a direct opencode binary is probed with --version, undecidable → true.
func openCodeIsV2(inner []string) bool {
	cmd := sandboxrun.UnwrapEnv(inner)
	for len(cmd) > 0 && strings.HasPrefix(cmd[0], "-") {
		cmd = cmd[1:]
	}
	if len(cmd) == 0 || cmd[0] == "" {
		return false
	}
	if maj, ok := openCodeSpecVersion(cmd[1:]); ok {
		return maj != 1
	}
	switch filepath.Base(cmd[0]) {
	case "opencode", "opencode-ai":
		// Direct binary: probe its version below.
	default:
		// Known wrapper without spec: undecidable → pin, a missed v2 pin fails loudly while a redundant v1 pin is benign.
		return openCodeWrappers[filepath.Base(cmd[0])]
	}
	bin, err := exec.LookPath(cmd[0])
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	if err != nil || len(out) == 0 {
		return true
	}
	m := openCodeVersionRe.FindStringSubmatch(string(out))
	if m == nil {
		return true
	}
	major, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return true
	}
	return major != 1
}

// openCodeSpecVersion extracts the pinned major from a versioned opencode spec among args (bunx opencode-ai@1.2.0 → 1).
func openCodeSpecVersion(args []string) (int, bool) {
	for _, arg := range args {
		m := openCodeSpecRe.FindStringSubmatch(arg)
		if m == nil {
			continue
		}
		if major, err := strconv.Atoi(m[1]); err == nil {
			return major, true
		}
	}
	return 0, false
}

// openCodePinDenied hard-errors when the profile's deny_vars would strip a var the pin's config/state redirects rely on.
func openCodePinDenied(plan sandboxPlan) error {
	if plan.Policy == nil {
		return nil
	}
	profile := plan.PolicyRef
	if profile == "" {
		profile = plan.Name
	}
	for _, v := range []string{"XDG_STATE_HOME", "OPENCODE_CONFIG_DIR"} {
		if sandboxprofile.EnvVarMatches(v, plan.Policy.Environment.DenyVars) {
			return fmt.Errorf("profile %q: deny_vars denies %s, which the opencode v2 service pin needs; the pin's redirects would be stripped and the sandboxed service would revert to its ungranted default port. Remove %s from the profile's deny_vars.", profile, v, v)
		}
	}
	return nil
}
