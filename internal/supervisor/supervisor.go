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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
)

// SidecarSpec is the supervisor's view of one sidecar to run.
type SidecarSpec struct {
	// Name is the supervisor's unique tracking key for this sidecar (used
	// by StopSidecar and log filenames). In serve mode it may be a
	// namespaced value like "__global__/skill-marketplace" so two
	// directories' same-named skills stay distinct.
	Name string
	// SkillName is the plain skill name exposed to the sidecar as
	// SIDECAR_SKILL. It must NOT contain a namespace prefix or any path
	// separator, because sidecars commonly use it to build filesystem
	// paths (e.g. tempfile prefixes). When empty, Name is used (the
	// single-workdir `start` case, where Name is already the plain name).
	SkillName string
	// Namespace is the facade namespace this sidecar is mounted under
	// ("" flat/start, a dir token, or "__global__"). Used only for audit
	// events (it is hashed before being written). Optional.
	Namespace      string
	SkillDir       string // absolute
	Command        []string
	EnvPassthrough []string
	Secrets        map[string]secrets.Secret // name → value
	// Config holds non-secret values from .opencode/skill-config.yaml
	// keyed by env-var name. These are injected into the sidecar's
	// environment alongside Secrets but are surfaced from a plain JSON
	// file, not the OS keychain. Secrets take precedence on collision
	// (also enforced at meta-validation time, so the conflict shouldn't
	// reach this point).
	Config  map[string]string
	Health  config.HealthSpec
	LogPath string
	Workdir string // host workdir
	// HarnessSkillsDir is the active harness's workdir-relative skills
	// directory (e.g. ".opencode/skills", ".claude/skills"), injected as
	// OMAC_HARNESS_SKILLS_DIR so skills that install into the project (the
	// marketplace) default to the dir the running harness loads. Empty when
	// no harness context is available.
	HarnessSkillsDir string
}

// Running represents a started sidecar.
type Running struct {
	Name    string
	Port    int
	Cmd     *exec.Cmd
	LogFile *os.File

	// audit bookkeeping (unexported): captured at spawn so process.exit
	// can report duration and identity when the child is terminated.
	startedAt      time.Time
	auditSkill     string
	auditNamespace string
	exitOnce       sync.Once

	// waitDone is closed by watchChild once it has reaped Cmd (Cmd.Wait
	// returned). terminate() waits on this instead of calling Cmd.Wait
	// itself: os/exec.Cmd.Wait must only ever have one caller in flight,
	// and a second concurrent caller can block forever (see watchChild).
	waitDone chan struct{}
}

// Supervisor coordinates all sidecars.
type Supervisor struct {
	baseEnvPassthrough []string
	auditor            audit.Auditor

	mu       sync.Mutex
	children []*Running
}

// New returns a fresh Supervisor. auditor may be nil (a no-op auditor is
// substituted) so existing callers/tests keep working.
func New(baseEnvPassthrough []string, auditor audit.Auditor) *Supervisor {
	if auditor == nil {
		auditor = audit.Nop()
	}
	return &Supervisor{baseEnvPassthrough: baseEnvPassthrough, auditor: auditor}
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

// AddSidecar starts a single sidecar at runtime and tracks it. Used by
// serve mode to bring a directory's skills (or a global skill) online
// lazily, after StartAll has already run (or instead of it). Safe to call
// concurrently. On health-check failure the child is torn down and the
// error returned; nothing is added to the tracked set.
func (s *Supervisor) AddSidecar(ctx context.Context, spec SidecarSpec) (*Running, error) {
	r, err := s.startOne(ctx, spec)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.children = append(s.children, r)
	s.mu.Unlock()
	return r, nil
}

// StopSidecar terminates the tracked sidecar with the given name and
// removes it from the tracked set. A no-op (returns false) if no such
// sidecar is tracked. Used by serve mode on directory deactivation and
// when swapping a stub for a live route.
func (s *Supervisor) StopSidecar(name string, timeout time.Duration) bool {
	s.mu.Lock()
	var target *Running
	keep := s.children[:0:0]
	for _, r := range s.children {
		if r.Name == name && target == nil {
			target = r
			continue
		}
		keep = append(keep, r)
	}
	if target != nil {
		s.children = keep
	}
	s.mu.Unlock()
	if target == nil {
		return false
	}
	_ = terminate(target.Cmd, timeout, target.waitDone)
	s.auditExit(target) // closes LogFile via exitOnce
	return true
}

// auditExit emits a process.exit event for a terminated child and closes
// its log file. Safe to call once per child after terminate() has reaped
// it. Idempotent via exitOnce: if watchChild already fired (self-terminated
// child), this is a no-op. LogFile.Close is centralized here so the fd is
// released exactly once regardless of which path reaps the child.
func (s *Supervisor) auditExit(r *Running) {
	if r == nil {
		return
	}
	r.exitOnce.Do(func() {
		code := -1
		if r.Cmd != nil && r.Cmd.ProcessState != nil {
			code = r.Cmd.ProcessState.ExitCode()
		}
		var durMS int64
		if !r.startedAt.IsZero() {
			durMS = time.Since(r.startedAt).Milliseconds()
		}
		s.auditor.Emit(audit.ProcessExit(r.auditSkill, r.auditNamespace, code, durMS))
		if r.LogFile != nil {
			_ = r.LogFile.Close()
		}
	})
}

// watchChild spawns a reaper goroutine for a sidecar. When the child
// terminates on its own (before StopSidecar/ShutdownAll is called), the
// reaper reaps it via cmd.Wait() and calls auditExit (which emits
// process.exit + closes the log file) so the trail reflects actual
// termination time, not time-to-teardown. auditExit is idempotent via
// exitOnce: if StopSidecar/ShutdownAll later call auditExit on an
// already-reaped child, it's a no-op.
//
// watchChild is the sole owner of r.Cmd.Wait() for this child's lifetime.
// terminate() must never call Wait() itself on a child that has a running
// watchChild reaper (i.e. anything reachable via s.children) — it waits on
// r.waitDone instead. Calling Cmd.Wait() concurrently from two goroutines
// is unsupported by os/exec and can block the second caller forever, even
// after the process has actually exited.
func (s *Supervisor) watchChild(r *Running) {
	if r == nil || r.Cmd == nil || r.Cmd.Process == nil {
		return
	}
	r.waitDone = make(chan struct{})
	go func() {
		_ = r.Cmd.Wait()
		close(r.waitDone)
		s.auditExit(r)
	}()
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

	// Some skills declare command: ["./scripts/sidecar.py"] and rely on the
	// script being executable (shebang). Installers (e.g. the marketplace)
	// don't always preserve/set the execute bit when unpacking, which makes
	// the spawn fail with "permission denied". omac owns the spawn, so make
	// a relative in-skill script executable before exec'ing it — no manual
	// `chmod +x` required after install.
	ensureExecutable(spec.SkillDir, argv[0])

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
	// Log the command and PATH to the sidecar log file for diagnostics.
	fmt.Fprintf(lf, "[omac] sidecar %s starting: argv=%v cwd=%s\n", spec.Name, argv, spec.SkillDir)
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "PATH=") {
			fmt.Fprintf(lf, "[omac] sidecar PATH=%s\n", kv[5:])
		}
		if strings.HasPrefix(kv, "SIDECAR_PORT=") {
			fmt.Fprintf(lf, "[omac] sidecar SIDECAR_PORT=%s\n", kv[13:])
		}
	}

	// Audit: record the spawn (argv redacted downstream), the injected
	// secret/config NAMES (never values), and — separately — a
	// secret.inject event so the "which skill got which secret names"
	// question is answerable. skillName is the plain name; namespace is
	// hashed by the auditor.
	skillName := spec.SkillName
	if skillName == "" {
		skillName = spec.Name
	}
	secretNames := mapKeys(spec.Secrets)
	configNames := mapKeysStr(spec.Config)
	s.auditor.Emit(audit.ProcessExec(skillName, spec.Namespace, spec.SkillDir, argv, secretNames, configNames))
	if len(secretNames) > 0 || len(configNames) > 0 {
		s.auditor.Emit(audit.SecretInject(skillName, spec.Namespace, secretNames, configNames))
	}

	r := &Running{Name: spec.Name, Port: port, Cmd: cmd, LogFile: lf}
	// Capture audit bookkeeping for the reaper + process.exit.
	r.startedAt = time.Now()
	r.auditSkill = skillName
	r.auditNamespace = spec.Namespace

	if err := waitHealth(ctx, port, spec.Health); err != nil {
		// No watchChild reaper is running yet for this child (it only
		// starts after the health check passes below), so terminate must
		// reap it itself: pass nil.
		_ = terminate(cmd, 3*time.Second, nil)
		lf.Close()
		return nil, fmt.Errorf("%s: health: %w", spec.Name, err)
	}
	// Start the reaper only after health check passes, so a sidecar that
	// fails to start is handled by the error path (terminate + lf.Close)
	// without the reaper racing to emit process.exit / close lf.
	s.watchChild(r)
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
	skillName := spec.SkillName
	if skillName == "" {
		skillName = spec.Name
	}
	vars["SIDECAR_SKILL"] = skillName
	vars["OMAC_WORKDIR"] = spec.Workdir
	if spec.HarnessSkillsDir != "" {
		vars["OMAC_HARNESS_SKILLS_DIR"] = spec.HarnessSkillsDir
	}

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
			_ = terminate(r.Cmd, timeout, r.waitDone)
			s.auditExit(r) // closes LogFile via exitOnce
		}()
	}
	wg.Wait()
}

// terminate sends SIGTERM to the child's process group, waits up to timeout,
// then sends SIGKILL. reaped, when non-nil, is a channel already being
// closed by a watchChild reaper once it reaps cmd; terminate waits on it
// instead of calling cmd.Wait() itself, since Wait must only ever have one
// caller in flight (a second concurrent caller can hang forever). Pass nil
// only when no reaper is running yet for cmd (e.g. a health-check failure
// during startup, before watchChild is started).
func terminate(cmd *exec.Cmd, timeout time.Duration, reaped <-chan struct{}) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	done := reaped
	if done == nil {
		ch := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(ch)
		}()
		done = ch
	}
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

// ensureExecutable makes a relative in-skill script executable so it can be
// exec'd directly (command: ["./scripts/sidecar.py"]). It is a no-op for
// absolute paths and bare interpreter names (e.g. "python3", resolved on
// PATH) — only a path that resolves to an existing regular file *inside*
// skillDir is touched, and only to add the owner-execute bit if missing.
func ensureExecutable(skillDir, exe string) {
	// Bare command (interpreter on PATH) — nothing in the skill to chmod.
	if !strings.Contains(exe, "/") {
		return
	}
	path := exe
	if !filepath.IsAbs(path) {
		path = filepath.Join(skillDir, exe)
	}
	// Confine to the skill directory: don't chmod arbitrary absolute paths.
	rel, err := filepath.Rel(skillDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	if info.Mode()&0o100 != 0 {
		return // already owner-executable
	}
	_ = os.Chmod(path, info.Mode()|0o100)
}

// mapKeys returns the sorted keys of a secrets map (names only).
func mapKeys(m map[string]secrets.Secret) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mapKeysStr returns the sorted keys of a string map.
func mapKeysStr(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
