package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors of the Manager. Their texts already name the omac vm and
// the command that helps, so they are returned without a further prefix.
var (
	ErrNotCreated       = errors.New("omac vm is not created; run `omac vm up`")
	ErrNotRunning       = errors.New("omac vm is not running; run `omac vm up`")
	ErrOnline           = errors.New("omac vm is online; run `omac vm offline` before attaching")
	ErrSessionsAttached = errors.New("sessions are attached to the omac vm")
	ErrFirewallMissing  = errors.New("omac vm firewall omac-vmguard is not loaded; run `omac vm offline`")
)

const (
	defaultProvisionTimeout = 15 * time.Minute
	defaultRestartTimeout   = 5 * time.Minute
	defaultProbeInterval    = 2 * time.Second
)

// Deps are the external effects of the Manager; RealDeps wires production.
type Deps struct {
	Lima     LimaCtl
	SSH      SSHRunner
	Run      CommandRunner // ssh-keygen
	Probe    DockerProbe
	Pull     ImagePuller
	LookPath func(string) (string, error)
	PortFree func(int) bool
	Host     HostResources
	Alive    func(int) bool
	HostUser string // host login name, mapped to the VM user by AgentUserName
	Now      func() time.Time

	// Zero means 15m, 5m and 2s.
	ProvisionTimeout, RestartTimeout, ProbeInterval time.Duration
}

// RealDeps returns the production dependencies.
func RealDeps() Deps {
	return Deps{
		Lima:     DefaultLimaCtl,
		SSH:      DefaultSSH,
		Run:      ExecRunner,
		Probe:    DefaultDockerProbe,
		Pull:     CranePull,
		LookPath: exec.LookPath,
		PortFree: PortFree,
		Host:     DetectHost(),
		Alive:    ProcessAlive,
		HostUser: hostUserName(),
		Now:      time.Now,
	}
}

func hostUserName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// Manager runs the omac VM lifecycle. Out receives user-facing progress
// text; Log receives debug lines (limactl and ssh invocations).
type Manager struct {
	Layout     Layout
	ConfigPath string
	Deps       Deps
	Out        io.Writer
	Log        func(string, ...any)
}

// NewManager fills the zero timeouts with their defaults; a nil out discards.
func NewManager(l Layout, configPath string, deps Deps, out io.Writer) *Manager {
	if out == nil {
		out = io.Discard
	}
	if deps.ProvisionTimeout <= 0 {
		deps.ProvisionTimeout = defaultProvisionTimeout
	}
	if deps.RestartTimeout <= 0 {
		deps.RestartTimeout = defaultRestartTimeout
	}
	if deps.ProbeInterval <= 0 {
		deps.ProbeInterval = defaultProbeInterval
	}
	return &Manager{Layout: l, ConfigPath: configPath, Deps: deps, Out: out, Log: func(string, ...any) {}}
}

// Remote commands. The test fakes match these exact strings, so they are
// built in one place.
const (
	verifyFirewallCmd = "nft list table inet " + GuardTable
	markerPresentCmd  = "test -f " + ProvisionedMarker
	// bootDoneCmd succeeds once Lima finished every provisioning script of
	// the current boot; `limactl start` waits for the same file.
	bootDoneCmd = "test -f /run/lima-boot-done"
)

// writeModeCmd sets ModeFile, which the boot script reads on the next start.
// mode is ModeOnline or ModeOffline, so it needs no quoting.
func writeModeCmd(mode string) string {
	return "printf '%s' " + mode + " > " + ModeFile
}

// reprovisionCmd makes the next boot run the full system provisioning again
// with pkgs, online.
func reprovisionCmd(pkgs []string) string {
	return `printf '%s\n' ` + ShellQuote(strings.Join(pkgs, " ")) + " > " + PackagesFile +
		" && rm -f " + ProvisionedMarker + " && " + writeModeCmd(ModeOnline)
}

// dockerCmd addresses rootless dockerd over its TCP socket: a non-interactive
// ssh command does not read the DOCKER_HOST export in ~/.bashrc.
func dockerCmd(args string) string {
	return "docker -H tcp://127.0.0.1:" + strconv.Itoa(GuestDockerPort) + " " + args
}

func (m *Manager) agentTarget(st *State) SSHTarget {
	return SSHTarget{User: AgentUserName(m.Deps.HostUser), Port: st.SSHPort, KeyPath: m.Layout.AgentKey, KnownHosts: m.Layout.KnownHosts}
}

func (m *Manager) rootTarget(st *State) SSHTarget {
	return SSHTarget{User: "root", Port: st.SSHPort, KeyPath: m.Layout.AdminKey, KnownHosts: m.Layout.KnownHosts}
}

// Up creates, provisions and starts the VM, ending offline with the
// firewall verified. With reprovision, an existing VM reinstalls its
// packages from vm.yaml. A VM whose state says online is left online
// unless its provisioning never finished; see upOnline.
func (m *Manager) Up(ctx context.Context, reprovision bool) error {
	if _, err := FindLimactl(m.Deps.LookPath); err != nil {
		return fmt.Errorf("omac vm: %w", err)
	}
	cfg, err := LoadUserConfig(m.ConfigPath)
	if err != nil {
		return fmt.Errorf("omac vm: %w", err)
	}
	res := ResolveResources(cfg, m.Deps.Host)
	if err := EnsureKeys(ctx, m.Layout, m.Deps.Run); err != nil {
		return err
	}
	inst, err := m.inspect(ctx)
	if err != nil {
		return err
	}
	st, err := m.loadOrInitState(cfg, inst != nil)
	if err != nil {
		return err
	}
	running := inst != nil && inst.Status == StatusRunning
	switch {
	case inst == nil:
		return m.create(ctx, st, cfg, res)
	case reprovision:
		return m.reprovision(ctx, st, cfg, running)
	case st.Mode != ModeOffline:
		return m.upOnline(ctx, st, cfg, running)
	case !running:
		fmt.Fprintln(m.Out, "omac vm: starting (offline)…")
		return m.startOffline(ctx, st)
	default:
		r, err := m.report(ctx, inst, st)
		if err != nil {
			return err
		}
		r.Write(m.Out)
		return nil
	}
}

// loadOrInitState picks fresh ports only for a VM that does not exist yet:
// an existing instance has its ports baked into its lima.yaml.
func (m *Manager) loadOrInitState(cfg UserConfig, instanceExists bool) (*State, error) {
	if instanceExists {
		return m.loadState()
	}
	st, err := LoadState(m.Layout.StatePath)
	if err != nil || st != nil {
		return st, err
	}
	sshPort, dockerPort, err := PickPorts(m.Deps.PortFree)
	if err != nil {
		return nil, err
	}
	st = &State{Version: StateVersion, SSHPort: sshPort, DockerPort: dockerPort, Mode: ModeOffline, Packages: cfg.Packages}
	return st, SaveState(m.Layout.StatePath, st)
}

func (m *Manager) create(ctx context.Context, st *State, cfg UserConfig, res Resources) error {
	adminPub, err := ReadPubKey(m.Layout.AdminKey)
	if err != nil {
		return err
	}
	agentPub, err := ReadPubKey(m.Layout.AgentKey)
	if err != nil {
		return err
	}
	yaml, err := RenderLimaConfig(LimaInputs{
		Resources:   res,
		SSHPort:     st.SSHPort,
		DockerPort:  st.DockerPort,
		AgentUser:   AgentUserName(m.Deps.HostUser),
		AdminPubKey: adminPub,
		AgentPubKey: agentPub,
		Packages:    cfg.Packages,
		Ruleset:     Ruleset(),
	})
	if err != nil {
		return fmt.Errorf("omac vm: render lima config: %w", err)
	}
	if err := os.MkdirAll(m.Layout.LimaHome, 0o700); err != nil {
		return fmt.Errorf("omac vm: create %s: %w", m.Layout.LimaHome, err)
	}
	if err := os.WriteFile(m.Layout.LimaYAML, []byte(yaml), 0o600); err != nil {
		return fmt.Errorf("omac vm: write lima config: %w", err)
	}
	fmt.Fprintln(m.Out, "omac vm: creating and provisioning the VM (this takes a few minutes)…")
	if err := m.lima(ctx, "create", "--tty=false", "--name", InstanceName, m.Layout.LimaYAML); err != nil {
		return err
	}
	// The first boot runs online (nothing provisioned yet); record that
	// before anything can fail. Lists of a VM deleted outside omac go.
	st.Mode = ModeOnline
	st.Packages = cfg.Packages
	st.Images, st.JDKs, st.Worktrees = nil, nil, nil
	if err := SaveState(m.Layout.StatePath, st); err != nil {
		return err
	}
	if err := m.start(ctx, m.Deps.ProvisionTimeout); err != nil {
		return fmt.Errorf("%w; %s", err, retryUpHint)
	}
	if err := m.waitDocker(ctx, st, m.Deps.ProvisionTimeout); err != nil {
		return fmt.Errorf("%w; %s", err, retryUpHint)
	}
	return m.switchMode(ctx, st, ModeOffline, true, m.Deps.RestartTimeout)
}

const retryUpHint = "run `omac vm up` again (or `omac vm up --reprovision`)"

// upOnline handles a VM whose state says online: either the user switched
// online, or a provisioning was interrupted (state stays online until an
// offline switch verified the firewall), or a provisioning boot is still
// running after limactl start gave up waiting for it. A missing Lima
// boot-done file means the last case: up waits instead of restarting the
// VM in the middle of apt. Otherwise the guest's ProvisionedMarker tells
// the first two apart. Any failure of the marker check counts as missing,
// which errs towards re-provisioning rather than leaving apt locked out by
// an offline switch.
func (m *Manager) upOnline(ctx context.Context, st *State, cfg UserConfig, running bool) error {
	// This path may re-run provisioning online, so it refuses up front.
	if err := m.markOnlineUnattached(st); err != nil {
		return err
	}
	if !running {
		if err := m.start(ctx, m.Deps.RestartTimeout); err != nil {
			return err
		}
	}
	if m.ssh(ctx, m.rootTarget(st), nil, nil, bootDoneCmd) != nil {
		fmt.Fprintln(m.Out, "omac vm: provisioning is still running, waiting")
		if err := m.waitDocker(ctx, st, m.Deps.ProvisionTimeout); err != nil {
			return fmt.Errorf("%w; %s", err, retryUpHint)
		}
		if m.ssh(ctx, m.rootTarget(st), nil, nil, markerPresentCmd) == nil {
			return m.switchMode(ctx, st, ModeOffline, true, m.Deps.RestartTimeout)
		}
	}
	if m.ssh(ctx, m.rootTarget(st), nil, nil, markerPresentCmd) == nil {
		fmt.Fprintln(m.Out, "omac vm: VM is online; run `omac vm offline` before attaching sessions")
		return nil
	}
	fmt.Fprintln(m.Out, "omac vm: provisioning was not completed, re-running it")
	return m.reprovision(ctx, st, cfg, true)
}

// startOffline boots a stopped VM whose state says offline. The root-owned
// mode file already says offline (or is absent, which means offline once
// provisioned), so one start suffices; only if the firewall is then
// missing does it fall back to the full write-mode cycle.
func (m *Manager) startOffline(ctx context.Context, st *State) error {
	if err := m.start(ctx, m.Deps.RestartTimeout); err != nil {
		return err
	}
	if err := m.waitDocker(ctx, st, m.Deps.RestartTimeout); err != nil {
		return err
	}
	if err := m.verifyMode(ctx, st, ModeOffline); err != nil {
		fmt.Fprintln(m.Out, "omac vm: firewall not loaded after start, rewriting the mode and restarting")
		return m.switchMode(ctx, st, ModeOffline, true, m.Deps.RestartTimeout)
	}
	fmt.Fprintln(m.Out, "omac vm: running, offline")
	return nil
}

func (m *Manager) reprovision(ctx context.Context, st *State, cfg UserConfig, running bool) error {
	for _, p := range cfg.Packages {
		if !packagePattern.MatchString(p) {
			return fmt.Errorf("omac vm: package name %q has characters outside [A-Za-z0-9.+-]", p)
		}
	}
	if err := m.markOnlineUnattached(st); err != nil {
		return err
	}
	fmt.Fprintln(m.Out, "omac vm: reprovisioning (this takes a few minutes)…")
	if !running {
		// The VM boots offline; root ssh works regardless.
		if err := m.start(ctx, m.Deps.RestartTimeout); err != nil {
			return err
		}
	}
	st.Mode = ModeOnline
	if err := SaveState(m.Layout.StatePath, st); err != nil {
		return err
	}
	if err := m.ssh(ctx, m.rootTarget(st), nil, nil, reprovisionCmd(cfg.Packages)); err != nil {
		return fmt.Errorf("omac vm: reset provisioning: %w", err)
	}
	if err := m.switchMode(ctx, st, ModeOnline, true, m.Deps.ProvisionTimeout); err != nil {
		return err
	}
	st.Packages = cfg.Packages
	if err := SaveState(m.Layout.StatePath, st); err != nil {
		return err
	}
	return m.switchMode(ctx, st, ModeOffline, true, m.Deps.RestartTimeout)
}

// switchMode writes ModeFile, restarts the VM, waits for Docker and checks
// the firewall matches mode. State records ModeOnline until the check
// passes, so a failed switch never leaves state claiming offline.
func (m *Manager) switchMode(ctx context.Context, st *State, mode string, running bool, timeout time.Duration) error {
	if st.Mode != ModeOnline {
		st.Mode = ModeOnline
		if err := SaveState(m.Layout.StatePath, st); err != nil {
			return err
		}
	}
	if !running {
		if err := m.start(ctx, m.Deps.RestartTimeout); err != nil {
			return err
		}
	}
	if err := m.ssh(ctx, m.rootTarget(st), nil, nil, writeModeCmd(mode)); err != nil {
		return fmt.Errorf("omac vm: write %s: %w", ModeFile, err)
	}
	if err := m.lima(ctx, "stop", InstanceName); err != nil {
		return err
	}
	if err := m.start(ctx, timeout); err != nil {
		return err
	}
	if err := m.waitDocker(ctx, st, timeout); err != nil {
		return err
	}
	if err := m.verifyMode(ctx, st, mode); err != nil {
		return err
	}
	st.Mode = mode
	if err := SaveState(m.Layout.StatePath, st); err != nil {
		return err
	}
	fmt.Fprintf(m.Out, "omac vm: running, %s\n", mode)
	return nil
}

// verifyMode checks the omac-vmguard table: loaded offline, absent online.
func (m *Manager) verifyMode(ctx context.Context, st *State, mode string) error {
	err := m.checkFirewall(ctx, st)
	switch {
	case mode == ModeOffline && err != nil:
		return err
	case mode == ModeOnline && err == nil:
		return fmt.Errorf("omac vm: firewall %s is still loaded after switching online", GuardTable)
	}
	return nil
}

func (m *Manager) checkFirewall(ctx context.Context, st *State) error {
	if err := m.ssh(ctx, m.rootTarget(st), nil, nil, verifyFirewallCmd); err != nil {
		return fmt.Errorf("%w (%s: %v)", ErrFirewallMissing, verifyFirewallCmd, err)
	}
	return nil
}

// Down stops the VM. A stopped VM is left as it is.
func (m *Manager) Down(ctx context.Context) error {
	inst, err := m.instance(ctx)
	if err != nil {
		return err
	}
	if inst == nil {
		return ErrNotCreated
	}
	if inst.Status != StatusRunning {
		fmt.Fprintln(m.Out, "omac vm: already stopped")
		return nil
	}
	return m.lima(ctx, "stop", InstanceName)
}

// Reset deletes the VM and all host-side state below Root (keys, state,
// images, Lima home), then runs Up, which creates the VM online. It refuses
// while sessions are attached and keeps their markers.
func (m *Manager) Reset(ctx context.Context) error {
	// A state.json that does not load is deleted below anyway.
	st, err := LoadState(m.Layout.StatePath)
	if err != nil {
		st = nil
	}
	if err := m.markOnlineUnattached(st); err != nil {
		return err
	}
	inst, err := m.instance(ctx)
	if err != nil {
		return err
	}
	if inst != nil {
		if err := m.lima(ctx, "delete", "-f", InstanceName); err != nil {
			return err
		}
	}
	l := m.Layout
	for _, p := range []string{
		l.LimaHome, l.StatePath, l.KnownHosts, l.LimaYAML, m.imagesDir(),
		l.AdminKey, l.AdminKey + ".pub", l.AgentKey, l.AgentKey + ".pub",
	} {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("omac vm: remove %s: %w", p, err)
		}
	}
	return m.Up(ctx, false)
}

// SetMode switches to ModeOnline or ModeOffline with a restart. Going
// online is refused while sessions are attached.
func (m *Manager) SetMode(ctx context.Context, mode string) error {
	if mode != ModeOnline && mode != ModeOffline {
		return fmt.Errorf("omac vm: unknown mode %q (want %s or %s)", mode, ModeOnline, ModeOffline)
	}
	if mode == ModeOnline {
		if err := m.refuseAttached(); err != nil {
			return err
		}
	}
	inst, err := m.instance(ctx)
	if err != nil {
		return err
	}
	if inst == nil {
		return ErrNotCreated
	}
	st, err := m.loadState()
	if err != nil {
		return err
	}
	if mode == ModeOnline {
		if err := m.markOnlineUnattached(st); err != nil {
			return err
		}
	}
	fmt.Fprintf(m.Out, "omac vm: switching %s (restarts the VM)…\n", mode)
	return m.switchMode(ctx, st, mode, inst.Status == StatusRunning, m.Deps.RestartTimeout)
}

// markOnlineUnattached guards every path that takes the VM online. It
// refuses while a session is attached, records ModeOnline in st, and checks
// again: a session registers before it reads the mode, and once state says
// online new sessions refuse to attach, so the second check catches any
// session that registered after the first one. On a hit the previous mode
// is restored. With a nil st (no state.json) only the first check runs.
func (m *Manager) markOnlineUnattached(st *State) error {
	if err := m.refuseAttached(); err != nil {
		return err
	}
	if st == nil {
		return nil
	}
	prev := st.Mode
	st.Mode = ModeOnline
	if err := SaveState(m.Layout.StatePath, st); err != nil {
		return err
	}
	if err := m.refuseAttached(); err != nil {
		st.Mode = prev
		if serr := SaveState(m.Layout.StatePath, st); serr != nil {
			return errors.Join(err, serr)
		}
		return err
	}
	return nil
}

// refuseAttached returns ErrSessionsAttached while any session is attached.
func (m *Manager) refuseAttached() error {
	pids, err := AttachedSessions(m.Layout, m.Deps.Alive)
	if err != nil {
		return err
	}
	if len(pids) > 0 {
		return fmt.Errorf("omac vm: %w (pids %s); end them before going online", ErrSessionsAttached, joinInts(pids))
	}
	return nil
}

// AttachInfo is what a session needs to use the VM's Docker.
type AttachInfo struct {
	DockerPort, SSHPort int
	AgentKeyPath, User  string
	Docker              DockerVersion
}

// DockerHost is the DOCKER_HOST value for the forwarded port.
func (a AttachInfo) DockerHost() string {
	return "tcp://127.0.0.1:" + strconv.Itoa(a.DockerPort)
}

// PrepareAttach succeeds only for a running, offline VM whose firewall is
// loaded and whose forwarded port answers as Docker.
func (m *Manager) PrepareAttach(ctx context.Context) (AttachInfo, error) {
	st, err := m.runningState(ctx)
	if err != nil {
		return AttachInfo{}, err
	}
	if st.Mode != ModeOffline {
		return AttachInfo{}, ErrOnline
	}
	if err := m.checkFirewall(ctx, st); err != nil {
		return AttachInfo{}, err
	}
	v, err := m.Deps.Probe(ctx, st.DockerPort)
	if err != nil {
		return AttachInfo{}, fmt.Errorf("omac vm: docker on 127.0.0.1:%d: %w", st.DockerPort, err)
	}
	return AttachInfo{
		DockerPort:   st.DockerPort,
		SSHPort:      st.SSHPort,
		AgentKeyPath: m.Layout.AgentKey,
		User:         AgentUserName(m.Deps.HostUser),
		Docker:       v,
	}, nil
}

// instance resolves limactl and inspects the omac instance (nil when absent).
func (m *Manager) instance(ctx context.Context) (*Instance, error) {
	if _, err := FindLimactl(m.Deps.LookPath); err != nil {
		return nil, fmt.Errorf("omac vm: %w", err)
	}
	return m.inspect(ctx)
}

func (m *Manager) inspect(ctx context.Context) (*Instance, error) {
	if err := os.MkdirAll(m.Layout.LimaHome, 0o700); err != nil {
		return nil, fmt.Errorf("omac vm: create %s: %w", m.Layout.LimaHome, err)
	}
	m.Log("limactl list --json")
	return Inspect(ctx, m.Deps.Lima, m.Layout.LimaHome)
}

// runningState returns the state of a created, running VM.
func (m *Manager) runningState(ctx context.Context) (*State, error) {
	inst, err := m.instance(ctx)
	if err != nil {
		return nil, err
	}
	if inst == nil {
		return nil, ErrNotCreated
	}
	if inst.Status != StatusRunning {
		return nil, ErrNotRunning
	}
	return m.loadState()
}

// loadState reads state.json of an existing VM, where a missing file is an
// error.
func (m *Manager) loadState() (*State, error) {
	st, err := LoadState(m.Layout.StatePath)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, fmt.Errorf("omac vm: %s is missing; run `omac vm reset`", m.Layout.StatePath)
	}
	return st, nil
}

func (m *Manager) lima(ctx context.Context, args ...string) error {
	m.Log("limactl %s", strings.Join(args, " "))
	if err := m.Deps.Lima(ctx, m.Layout.LimaHome, nil, args...); err != nil {
		return fmt.Errorf("omac vm: limactl %s: %w", args[0], err)
	}
	return nil
}

// start boots the VM. limactl blocks until every provisioning script of the
// boot finished, so a boot that provisions needs the provisioning timeout;
// limactl's own default is shorter.
func (m *Manager) start(ctx context.Context, timeout time.Duration) error {
	return m.lima(ctx, "start", "--tty=false", "--timeout", timeout.String(), InstanceName)
}

func (m *Manager) waitDocker(ctx context.Context, st *State, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := WaitForDocker(ctx, st.DockerPort, m.Deps.ProbeInterval, m.Deps.Probe)
	if err != nil && !strings.HasPrefix(err.Error(), "omac vm:") {
		err = fmt.Errorf("omac vm: %w", err)
	}
	return err
}

// ssh runs remote and folds its stderr into the error.
func (m *Manager) ssh(ctx context.Context, t SSHTarget, stdin io.Reader, stdout io.Writer, remote string) error {
	m.Log("ssh %s@127.0.0.1 %s", t.User, remote)
	var stderr bytes.Buffer
	if err := m.Deps.SSH(ctx, t, stdin, stdout, &stderr, remote); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

func joinInts(xs []int) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = strconv.Itoa(x)
	}
	return strings.Join(s, ", ")
}
