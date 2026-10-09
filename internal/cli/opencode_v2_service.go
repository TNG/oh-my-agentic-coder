package cli

import (
	"context"
	"encoding/json"
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
	// hold keeps Port bound by omac from the pick until handOff, so no other process can take it during setup.
	hold net.Listener
	// hostPorts re-reads the host service's ports at handOff.
	hostPorts func() map[int]string
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

// release frees the reserved port; safe on a nil pin and when called twice.
func (p *openCodeServicePin) release() {
	if p == nil || p.hold == nil {
		return
	}
	_ = p.hold.Close()
	p.hold = nil
}

// handOff releases the reserved port right before exec and checks it is still free, so the window for another process is only the inner startup.
func (p *openCodeServicePin) handOff() error {
	p.release()
	var host map[int]string
	if p.hostPorts != nil {
		host = p.hostPorts()
	}
	return checkServicePortFree(p.Port, host)
}

// pinOpenCodeV2Service returns the pin for opencode v2+ inner commands, or nil for v1/other harnesses.
// The returned pin holds its port until handOff or release.
func pinOpenCodeV2Service(h config.Harness, inner []string, sandboxTmp string) (*openCodeServicePin, error) {
	if h.Name != "opencode" || sandboxTmp == "" {
		return nil, nil
	}
	if !openCodeIsV2(inner) {
		return nil, nil
	}
	hostPorts := func() map[int]string { return hostOpenCodeServicePorts(h) }
	port, hold, err := reserveServicePort(hostPorts())
	if err != nil {
		return nil, err
	}
	cfgDir, err := seedOpenCodeServiceConfig(h.ConfigHome(), filepath.Join(sandboxTmp, "opencode-config"), port)
	if err != nil {
		_ = hold.Close()
		return nil, err
	}
	return &openCodeServicePin{
		Port:      port,
		CfgDir:    cfgDir,
		StateDir:  filepath.Join(sandboxTmp, "opencode-state"),
		hold:      hold,
		hostPorts: hostPorts,
	}, nil
}

// openCodeDefaultServicePort is OpenCode v2's built-in shared-service port (0xc0de).
const openCodeDefaultServicePort = 49374

// openCodeServicePortEnv forces the pinned port; tests use it to simulate a taken port. A forced port gets the same checks.
const openCodeServicePortEnv = "OMAC_TEST_OPENCODE_SERVICE_PORT"

// servicePortError explains why the pinned port cannot be used.
type servicePortError struct {
	Port   int
	Reason string // completes "port N for the sandboxed OpenCode service ..."
	Holder string // "Python, pid 123" when known
}

func (e *servicePortError) Error() string {
	msg := fmt.Sprintf("port %d for the sandboxed OpenCode service %s", e.Port, e.Reason)
	if e.Holder != "" {
		msg += " (" + e.Holder + ")"
	}
	return msg + ". omac does not start OpenCode on a port it cannot have to itself. Free the port, or run the command again: omac picks a new port per launch."
}

func portInUse(port int) *servicePortError {
	return &servicePortError{Port: port, Reason: "is already in use by another process", Holder: portHolder(port)}
}

// reserveServicePort binds a free loopback port that is not one of the host service's ports and keeps it bound.
func reserveServicePort(host map[int]string) (int, net.Listener, error) {
	if forced := os.Getenv(openCodeServicePortEnv); forced != "" {
		port, err := strconv.Atoi(forced)
		if err != nil || port < 1 || port > 65535 {
			return 0, nil, fmt.Errorf("%s=%q is not a port", openCodeServicePortEnv, forced)
		}
		if reason, ok := host[port]; ok {
			return 0, nil, &servicePortError{Port: port, Reason: reason}
		}
		if portAnswers(port) {
			return 0, nil, portInUse(port)
		}
		l, err := net.Listen("tcp", loopbackAddr(port))
		if err != nil {
			return 0, nil, portInUse(port)
		}
		return port, l, nil
	}
	// Excluded picks stay bound until we return so the kernel does not hand them out again.
	var skipped []net.Listener
	defer func() {
		for _, l := range skipped {
			_ = l.Close()
		}
	}()
	for range 16 {
		l, err := listenAnyLoopback()
		if err != nil {
			return 0, nil, fmt.Errorf("pick service port: %w", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		if _, excluded := host[port]; excluded {
			skipped = append(skipped, l)
			continue
		}
		return port, l, nil
	}
	return 0, nil, fmt.Errorf("pick service port: no free loopback port outside the host OpenCode service's ports")
}

// listenAnyLoopback binds a kernel-chosen loopback port; a variable so tests can script the picks.
var listenAnyLoopback = func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

// checkServicePortFree refuses a port the host service owns, that something answers on, or that cannot be bound.
func checkServicePortFree(port int, host map[int]string) error {
	if reason, ok := host[port]; ok {
		return &servicePortError{Port: port, Reason: reason}
	}
	if portAnswers(port) {
		return portInUse(port)
	}
	l, err := net.Listen("tcp", loopbackAddr(port))
	if err != nil {
		return portInUse(port)
	}
	return l.Close()
}

// portAnswers reports whether something accepts TCP connections on the loopback port.
func portAnswers(port int) bool {
	conn, err := net.DialTimeout("tcp", loopbackAddr(port), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func loopbackAddr(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// portHolder names the process listening on port via lsof (always on macOS, on Linux when installed); "" when unknown.
func portHolder(port int) string {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, lsof, "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fpc").Output()
	return parseLsofHolder(string(out))
}

// parseLsofHolder turns lsof -Fpc output ("p123\ncPython\n") into "Python, pid 123", using the first process only.
func parseLsofHolder(out string) string {
	var pid, name string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			if pid != "" {
				return holderString(name, pid)
			}
			pid = line[1:]
		case 'c':
			if name == "" {
				name = line[1:]
			}
		}
	}
	return holderString(name, pid)
}

func holderString(name, pid string) string {
	switch {
	case name != "" && pid != "":
		return name + ", pid " + pid
	case pid != "":
		return "pid " + pid
	default:
		return name
	}
}

// openCodeServiceInfo is the part of OpenCode v2's service.json omac reads: config ({"port"}) and registration ({"url","pid","password"}).
type openCodeServiceInfo struct {
	Port     int    `json:"port"`
	URL      string `json:"url"`
	PID      int    `json:"pid"`
	Password string `json:"password"`
}

// port returns the explicit port, else the one in url, else 0.
func (s openCodeServiceInfo) port() int {
	if s.Port > 0 {
		return s.Port
	}
	u, err := url.Parse(s.URL)
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		return 0
	}
	return p
}

func readOpenCodeServiceInfo(path string) (openCodeServiceInfo, bool) {
	var s openCodeServiceInfo
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &s) != nil {
		return openCodeServiceInfo{}, false
	}
	return s, true
}

// hostOpenCodeServicePorts maps the ports the host's own OpenCode shared service uses or is configured for to why they are excluded.
func hostOpenCodeServicePorts(h config.Harness) map[int]string {
	ports := map[int]string{openCodeDefaultServicePort: "is OpenCode's default shared-service port"}
	for _, dir := range []string{h.ConfigHome(), os.Getenv("OPENCODE_CONFIG_DIR")} {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, "service.json")
		if s, ok := readOpenCodeServiceInfo(path); ok && s.Port > 0 {
			ports[s.Port] = "is the host OpenCode shared service's configured port (" + path + ")"
		}
	}
	if root := hostStateHome(); root != "" {
		path := filepath.Join(root, "opencode", "service.json")
		if s, ok := readOpenCodeServiceInfo(path); ok && s.port() > 0 {
			reason := "is registered by the host OpenCode shared service (" + path
			if s.PID > 0 {
				reason += fmt.Sprintf(", pid %d", s.PID)
			}
			ports[s.port()] = reason + ")"
		}
	}
	return ports
}

// hostStateHome is the host's XDG state root, where OpenCode registers its shared service.
func hostStateHome() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state")
}

// serviceCheck tunes verifyService; tests shorten it.
type serviceCheck struct {
	Poll   time.Duration // probe interval
	Grace  time.Duration // how long an unproven listener on the port is tolerated (covers bind → registration)
	Window time.Duration // how long after launch to wait for the service at all
}

var defaultServiceCheck = serviceCheck{Poll: 100 * time.Millisecond, Grace: 5 * time.Second, Window: 30 * time.Second}

// verifyService watches the pinned port after launch until the listener on it is proven to be the sandboxed service.
//
// Proof: the launch-private registration (StateDir/opencode/service.json, written only by the sandboxed service)
// reports the pinned port, and the service on that port answers /api/info for that registration's credentials with
// its pid. No process-tree walk: OpenCode detaches the service from its client (it is reparented once the client
// exits), and on Linux the sandbox's pid namespace hides host pids, so ancestry is not reliable on either OS.
// A listener that stays unproven for Grace is another process and the launch is refused. If nothing listens
// within Window (the command never started the service), it returns nil. pid is the proven service's pid, else 0.
func (p *openCodeServicePin) verifyService(ctx context.Context, c serviceCheck) (pid int, err error) {
	client := &http.Client{Timeout: time.Second}
	start := time.Now()
	var unproven time.Time
	for {
		if reg, ok := readOpenCodeServiceInfo(filepath.Join(p.StateDir, "opencode", "service.json")); ok {
			if got := reg.port(); got != p.Port {
				return 0, &servicePortError{Port: p.Port, Reason: fmt.Sprintf("was not used: the sandboxed OpenCode service registered port %d instead", got)}
			}
			if p.answersAs(ctx, client, reg) {
				return reg.PID, nil
			}
		}
		now := time.Now()
		if portAnswers(p.Port) {
			if unproven.IsZero() {
				unproven = now
			} else if now.Sub(unproven) >= c.Grace {
				e := portInUse(p.Port)
				e.Reason = "was taken by another process before the service could bind it"
				return 0, e
			}
		} else {
			unproven = time.Time{}
			if now.Sub(start) >= c.Window {
				return 0, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, nil
		case <-time.After(c.Poll):
		}
	}
}

// answersAs reports whether the service on the pinned port accepts reg's credentials and reports reg's pid.
func (p *openCodeServicePin) answersAs(ctx context.Context, client *http.Client, reg openCodeServiceInfo) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+loopbackAddr(p.Port)+"/api/info", nil)
	if err != nil {
		return false
	}
	req.SetBasicAuth("opencode", reg.Password)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var info struct {
		PID int `json:"pid"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&info); err != nil {
		return false
	}
	return info.PID != 0 && info.PID == reg.PID
}

// watch verifies the service while the sandbox runs and stops the sandbox (with the service it started) if another process holds the port.
// onReady receives the sandbox child's pid (it leads its own process group); finish ends the watch and returns
// the verdict and the proven service pid (0 if the service was not seen).
func (p *openCodeServicePin) watch(c serviceCheck) (onReady func(pid int), finish func() (int, error)) {
	ctx, cancel := context.WithCancel(context.Background())
	pidCh := make(chan int, 1)
	done := make(chan struct{})
	var servicePID int
	var verdict error
	go func() {
		defer close(done)
		servicePID, verdict = p.verifyService(ctx, c)
		if verdict == nil {
			return
		}
		select {
		case pid := <-pidCh:
			stopSandboxTree(ctx.Done(), pid)
		case <-ctx.Done():
		}
	}()
	onReady = func(pid int) { pidCh <- pid }
	finish = func() (int, error) {
		cancel()
		<-done
		return servicePID, verdict
	}
	return onReady, finish
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

// Match whole package arguments so similarly named packages cannot select a version.
var openCodeSpecRe = regexp.MustCompile(`^(?:--package=)?(?:@opencode/cli|opencode(?:-ai)?)@v?(\d+)(?:\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.-]+)?)?$`)

// openCodeIsV2 reports whether inner is opencode v2+; wrappers decide from a versioned package spec in their args, only a direct opencode binary is probed with --version, undecidable → true.
func openCodeIsV2(inner []string) bool {
	return openCodeV2(inner, true)
}

// Service pinning tolerates unknown versions; disabling project sources requires a confirmed version.
func openCodeV2(inner []string, unknown bool) bool {
	cmd := sandboxrun.UnwrapEnv(inner)
	for len(cmd) > 0 && strings.HasPrefix(cmd[0], "-") {
		cmd = cmd[1:]
	}
	if len(cmd) == 0 || cmd[0] == "" {
		return false
	}
	if openCodeWrappers[filepath.Base(cmd[0])] {
		if maj, ok := openCodeSpecVersion(cmd[1:]); ok {
			return maj != 1 && (unknown || maj >= 2)
		}
		return unknown
	}
	switch filepath.Base(cmd[0]) {
	case "opencode", "opencode-ai":
		// Direct binary: probe its version below.
	default:
		return false
	}
	bin, err := exec.LookPath(cmd[0])
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	if err != nil || len(out) == 0 {
		return unknown
	}
	m := openCodeVersionRe.FindStringSubmatch(string(out))
	if m == nil {
		return unknown
	}
	major, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return unknown
	}
	return major != 1 && (unknown || major >= 2)
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
			return fmt.Errorf("profile %q: deny_vars denies %s, which the opencode v2 service pin needs; the pin's redirects would be stripped and the sandboxed service would revert to its ungranted default port, remove %s from the profile's deny_vars", profile, v, v)
		}
	}
	return nil
}
