package ephemeraldocker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultBootTimeout bounds the whole first boot: image-backed boot,
// apk-installing docker, dockerd start, guest probe. Generous, because
// the failure mode (timeout) costs a full teardown and restart.
const DefaultBootTimeout = 5 * time.Minute

// Logger receives non-fatal diagnostics: sweep findings, limactl
// create/start exit codes (which are not authoritative), reap results.
type Logger func(format string, args ...any)

// SessionOpts carries the orchestration seams. Every functional
// dependency is injectable so the whole boot runs without a VM in tests;
// nil fields fall back to the real implementations.
type SessionOpts struct {
	// CacheDir is the resolved omac cache scope dir.
	CacheDir string
	// WorkDir is the canonical worktree path; it seeds the stable port.
	WorkDir string
	// Arch is the guest architecture (aarch64, x86_64).
	Arch string
	// BootTimeout bounds the endpoint wait; 0 = DefaultBootTimeout.
	BootTimeout time.Duration

	Run         LimaCtl
	Reap        Reaper
	Probe       EndpointProbe
	EnsureImage func(cacheDir, arch, urlOverride string) (ImageSpec, error)
	LookPath    func(string) (string, error)
	PortFree    func(int) bool
	Log         Logger
}

// Session is one running ephemeral-docker unit: a booted VM whose guest
// docker endpoint answers on the host forward, with the vmguard table
// demonstrably loaded.
type Session struct {
	VMName   string
	HostPort int
	Lay      *Layout

	opts     SessionOpts
	mu       sync.Mutex
	tornDown bool
}

// Handle is the CLI's view of one running session: enough to wire
// DOCKER_HOST and tear the unit down, without exposing the layout.
// *Session implements it.
type Handle interface {
	// Port is the loopback host port forwarding to the guest docker
	// endpoint; it must be opened into the sandbox (--open-port).
	Port() int
	// DockerHost is the value the sandboxed agent receives in
	// DOCKER_HOST.
	DockerHost() string
	// Teardown removes the whole unit (idempotent).
	Teardown() error
}

func (s *Session) Port() int { return s.HostPort }

// DockerHost is the value the sandboxed agent receives in DOCKER_HOST.
func (s *Session) DockerHost() string {
	return fmt.Sprintf("tcp://127.0.0.1:%d", s.HostPort)
}

func (o *SessionOpts) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// StartSession runs the whole boot orchestration: limactl check, orphan
// sweep (log-only failures), layout, image prefetch, Lima config write,
// create + start (exit codes logged, never authoritative), endpoint
// wait, firewall verification. Any failure after the layout triggers the
// full teardown and the session never reaches the caller.
func StartSession(ctx context.Context, opts SessionOpts) (*Session, error) {
	if opts.Run == nil {
		opts.Run = DefaultLimaCtl
	}
	if opts.Reap == nil {
		opts.Reap = DefaultReaper
	}
	if opts.Probe == nil {
		opts.Probe = DefaultEndpointProbe
	}
	if opts.EnsureImage == nil {
		opts.EnsureImage = EnsureImage
	}
	if opts.LookPath == nil {
		opts.LookPath = defaultLookPath
	}
	if opts.PortFree == nil {
		opts.PortFree = portIsFree
	}
	if opts.BootTimeout <= 0 {
		opts.BootTimeout = DefaultBootTimeout
	}

	// Missing limactl is a hard prerequisite; nothing exists yet, so
	// there is nothing to tear down.
	if _, err := FindLimactl(opts.LookPath); err != nil {
		return nil, err
	}

	// Orphan sweep: best effort. Failures are logged, not fatal — a
	// broken leftover unit must not block fresh sessions.
	root := filepath.Join(opts.CacheDir, scopeDirName)
	if res, errs := Sweep(root, opts.Run, opts.Reap); len(errs) > 0 || len(res.Reaped) > 0 || len(res.Active) > 0 {
		opts.logf("ephemeral-docker: sweep: %d orphaned unit(s) reaped, %d active", len(res.Reaped), len(res.Active))
		for _, e := range errs {
			opts.logf("ephemeral-docker: sweep error: %v", e)
		}
	}

	lay, err := NewLayout(opts.CacheDir, opts.WorkDir, opts.PortFree)
	if err != nil {
		return nil, err
	}
	s := &Session{VMName: lay.VMName, HostPort: lay.HostPort, Lay: lay, opts: opts}

	img, err := opts.EnsureImage(opts.CacheDir, opts.Arch, "")
	if err != nil {
		return nil, s.teardownJoin(err)
	}
	yaml, err := RenderLimaConfig(LimaConfig{
		VMName:   lay.VMName,
		HostPort: lay.HostPort,
		Arch:     opts.Arch,
		Image:    img,
		Ruleset:  Ruleset(),
	})
	if err != nil {
		return nil, s.teardownJoin(err)
	}
	if err := os.WriteFile(lay.LimaYAML, []byte(yaml), 0o600); err != nil {
		return nil, s.teardownJoin(fmt.Errorf("ephemeral-docker: write %s: %w", lay.LimaYAML, err))
	}

	// create/start exit codes are logged, not authoritative: lima is
	// known to report failure on paths where the VM still boots (protob
	// precedent). The endpoint and the firewall check decide alone.
	if err := opts.Run(lay.LimaHome, "create", "--name", lay.VMName, lay.LimaYAML); err != nil {
		opts.logf("ephemeral-docker: limactl create reported failure (endpoint decides): %v", err)
	}
	if err := opts.Run(lay.LimaHome, "start", "--tty=false", lay.VMName); err != nil {
		opts.logf("ephemeral-docker: limactl start reported failure (endpoint decides): %v", err)
	}

	bootCtx, cancel := context.WithTimeout(ctx, opts.BootTimeout)
	defer cancel()
	if state := WaitForEndpoint(bootCtx, lay.HostPort, 0, opts.Probe); state != EndpointReady {
		return nil, s.teardownJoin(fmt.Errorf(
			"ephemeral-docker: %s: docker endpoint did not answer on port %d within %s",
			lay.VMName, lay.HostPort, opts.BootTimeout))
	}
	if err := VerifyFirewall(opts.Run, lay.LimaHome, lay.VMName); err != nil {
		return nil, s.teardownJoin(err)
	}
	return s, nil
}

// Teardown removes the whole unit: limactl delete -f, QEMU reap, the
// short LIMA_HOME under /tmp, the bookkeeping dir in the scope, the
// liveness lock — then verifies no listener remains on the host port.
// Failures are joined; every error names the VM so the user and the next
// sweep can clean up. Idempotent.
func (s *Session) Teardown() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tornDown {
		return nil
	}
	s.tornDown = true

	var errs []error
	if err := s.opts.Run(s.Lay.LimaHome, "delete", "-f", s.VMName); err != nil {
		errs = append(errs, fmt.Errorf("ephemeral-docker: teardown of %s: limactl delete failed: %w", s.VMName, err))
	}
	if err := s.opts.Reap(s.VMName); err != nil {
		errs = append(errs, fmt.Errorf("ephemeral-docker: teardown of %s: QEMU reap failed: %w", s.VMName, err))
	}
	if err := os.RemoveAll(s.Lay.LimaHome); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("ephemeral-docker: teardown of %s: remove LIMA_HOME %s: %w", s.VMName, s.Lay.LimaHome, err))
	}
	if err := os.RemoveAll(s.Lay.Dir); err != nil {
		errs = append(errs, fmt.Errorf("ephemeral-docker: teardown of %s: remove state dir %s: %w", s.VMName, s.Lay.Dir, err))
	}
	_ = s.Lay.Release()
	if !s.opts.PortFree(s.HostPort) {
		errs = append(errs, fmt.Errorf("ephemeral-docker: teardown of %s: host port %d still has a listener", s.VMName, s.HostPort))
	}
	return errors.Join(errs...)
}

// teardownJoin tears the unit down and joins the cause with whatever the
// teardown itself failed at.
func (s *Session) teardownJoin(cause error) error {
	return errors.Join(cause, s.Teardown())
}
