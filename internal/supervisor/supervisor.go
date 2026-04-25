// Package supervisor spawns and health-checks sidecar processes.
//
// Each sidecar:
//   - gets an ephemeral 127.0.0.1 port allocated by the supervisor,
//   - is run with a hand-crafted env (base passthrough + allow-listed
//     user env + injected secrets + SIDECAR_PORT/SIDECAR_SKILL/OMAC_WORKDIR),
//   - has its stdio piped to a per-skill log file,
//   - is health-probed on sidecar.health.path until 2xx or timeout.
//
// Secrets are passed via env only; they never appear on argv.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
)

// SidecarSpec is the supervisor's view of one sidecar to run.
type SidecarSpec struct {
	Name           string
	SkillDir       string // absolute
	Command        []string
	EnvPassthrough []string
	Secrets        map[string]secrets.Secret // name → value
	// Config holds non-secret values from .opencode/skill-config.json
	// keyed by env-var name. These are injected into the sidecar's
	// environment alongside Secrets but are surfaced from a plain JSON
	// file, not the OS keychain. Secrets take precedence on collision
	// (also enforced at meta-validation time, so the conflict shouldn't
	// reach this point).
	Config  map[string]string
	Health  config.HealthSpec
	LogPath string
	Workdir string // host workdir
}

// Running represents a started sidecar.
type Running struct {
	Name    string
	Port    int
	Cmd     *exec.Cmd
	LogFile *os.File
}

// Supervisor coordinates all sidecars.
type Supervisor struct {
	baseEnvPassthrough []string

	mu       sync.Mutex
	children []*Running
}

// New returns a fresh Supervisor.
func New(baseEnvPassthrough []string) *Supervisor {
	return &Supervisor{baseEnvPassthrough: baseEnvPassthrough}
}

// StartAll starts every sidecar in specs. On any failure it terminates the
// ones already started and returns the original error.
func (s *Supervisor) StartAll(ctx context.Context, specs []SidecarSpec) ([]*Running, error) {
	out := make([]*Running, 0, len(specs))
	for _, spec := range specs {
		r, err := s.startOne(ctx, spec)
		if err != nil {
			s.ShutdownAll(5 * time.Second)
			return nil, err
		}
		out = append(out, r)
		s.mu.Lock()
		s.children = append(s.children, r)
		s.mu.Unlock()
	}
	return out, nil
}

// startOne allocates a port, spawns the child, and waits on health.
func (s *Supervisor) startOne(ctx context.Context, spec SidecarSpec) (*Running, error) {
	port, err := allocEphemeralPort()
	if err != nil {
		return nil, fmt.Errorf("%s: port alloc: %w", spec.Name, err)
	}

	if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o700); err != nil {
		return nil, fmt.Errorf("%s: mkdir logs: %w", spec.Name, err)
	}
	// Rotate previous log.
	if _, err := os.Stat(spec.LogPath); err == nil {
		_ = os.Rename(spec.LogPath, spec.LogPath+".1")
	}
	lf, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%s: open log: %w", spec.Name, err)
	}

	// Build argv with ${SIDECAR_PORT} expansion.
	argv := expandArgv(spec.Command, map[string]string{"SIDECAR_PORT": fmt.Sprint(port)})
	if len(argv) == 0 {
		lf.Close()
		return nil, fmt.Errorf("%s: empty command", spec.Name)
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = spec.SkillDir
	cmd.Env = s.buildEnv(spec, port)
	cmd.Stdout = lf
	cmd.Stderr = lf
	// A new process group so we can signal the entire child tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		lf.Close()
		return nil, fmt.Errorf("%s: start: %w", spec.Name, err)
	}

	r := &Running{Name: spec.Name, Port: port, Cmd: cmd, LogFile: lf}

	if err := waitHealth(ctx, port, spec.Health); err != nil {
		_ = terminate(cmd, 3*time.Second)
		lf.Close()
		return nil, fmt.Errorf("%s: health: %w", spec.Name, err)
	}
	return r, nil
}

// buildEnv constructs the sidecar's environment from scratch.
// Precedence (high → low): injected secrets > skill env_passthrough >
// facade base_env_passthrough > facade-injected vars.
func (s *Supervisor) buildEnv(spec SidecarSpec, port int) []string {
	vars := map[string]string{}

	host := os.Environ()
	hostMap := make(map[string]string, len(host))
	for _, kv := range host {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			hostMap[kv[:i]] = kv[i+1:]
		}
	}
	for _, k := range s.baseEnvPassthrough {
		if v, ok := hostMap[k]; ok {
			vars[k] = v
		}
	}
	for _, k := range spec.EnvPassthrough {
		if v, ok := hostMap[k]; ok {
			vars[k] = v
		}
	}
	// Facade-injected.
	vars["SIDECAR_PORT"] = fmt.Sprint(port)
	vars["SIDECAR_SKILL"] = spec.Name
	vars["OMAC_WORKDIR"] = spec.Workdir

	// Non-secret config fields. Win over passthrough; lose to secrets
	// (which is also a meta-validation-time error, so practically these
	// two maps share no keys).
	for name, v := range spec.Config {
		vars[name] = v
	}

	// Secrets — always win over passthrough and config.
	for name, s := range spec.Secrets {
		vars[name] = s.ExposeString()
	}

	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	return out
}

// ShutdownAll sends SIGTERM to every running child, waits up to timeout,
// then SIGKILL to the stragglers.
func (s *Supervisor) ShutdownAll(timeout time.Duration) {
	s.mu.Lock()
	children := s.children
	s.children = nil
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, r := range children {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = terminate(r.Cmd, timeout)
			if r.LogFile != nil {
				_ = r.LogFile.Close()
			}
		}()
	}
	wg.Wait()
}

// terminate sends SIGTERM to the child's process group, waits up to timeout,
// then sends SIGKILL.
func terminate(cmd *exec.Cmd, timeout time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
		return nil
	}
}

// waitHealth polls until the upstream returns 2xx on spec.Path or the
// overall timeout (initial_delay_ms + timeout_ms) elapses.
func waitHealth(ctx context.Context, port int, spec config.HealthSpec) error {
	spec = spec.Defaults()
	time.Sleep(time.Duration(spec.InitialDelayMS) * time.Millisecond)
	deadline := time.Now().Add(time.Duration(spec.TimeoutMS) * time.Millisecond)
	client := &http.Client{Timeout: 1 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, spec.Path)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(time.Duration(spec.IntervalMS) * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("timeout")
	}
	return lastErr
}

// allocEphemeralPort binds :0 on 127.0.0.1, remembers the port, and closes.
// Race with another bind is possible but rare; callers can retry.
func allocEphemeralPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port, nil
}

// expandArgv expands ${VAR} tokens inside argv elements from vars.
// Unknown vars expand to empty.
func expandArgv(argv []string, vars map[string]string) []string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		out = append(out, expand(a, vars))
	}
	return out
}

func expand(s string, vars map[string]string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == '$' && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end >= 0 {
				name := s[i+2 : i+2+end]
				b.WriteString(vars[name])
				i += 2 + end + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// CopyWriter keeps a reference to io.Writer to avoid unused-import warnings
// when certain build tags exclude parts of the file. Safe no-op.
var _ = io.Discard
