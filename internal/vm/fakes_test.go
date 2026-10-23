package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type limaCall struct {
	home string
	args []string
}

// fakeLimaCtl simulates limactl for the single omac instance. onStart and
// onStop let the fake guest react to boots and shutdowns.
type fakeLimaCtl struct {
	mu      sync.Mutex
	calls   []limaCall
	inst    *Instance // nil = not created
	failOn  map[string]error
	onStart func()
	onStop  func()
	// startTimeout makes the next start boot the guest and then fail, as
	// limactl does when its --timeout expires while the guest provisions.
	startTimeout error
}

func (f *fakeLimaCtl) run(_ context.Context, home string, stdout io.Writer, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, limaCall{home, slices.Clone(args)})
	if err := f.failOn[args[0]]; err != nil {
		return err
	}
	switch args[0] {
	case "list":
		if !slices.Equal(args, []string{"list", "--json"}) {
			return fmt.Errorf("fakeLimaCtl: unexpected list args %v", args)
		}
		if f.inst != nil && stdout != nil {
			return json.NewEncoder(stdout).Encode(f.inst)
		}
	case "create":
		if f.inst != nil {
			return errors.New("fakeLimaCtl: instance exists")
		}
		f.inst = &Instance{Name: InstanceName, Status: StatusStopped}
	case "start":
		if f.inst == nil {
			return errors.New("fakeLimaCtl: start: instance missing")
		}
		f.inst.Status = StatusRunning
		if f.onStart != nil {
			f.onStart()
		}
		if err := f.startTimeout; err != nil {
			f.startTimeout = nil
			return err
		}
	case "stop":
		if f.inst == nil {
			return errors.New("fakeLimaCtl: stop: instance missing")
		}
		f.inst.Status = StatusStopped
		if f.onStop != nil {
			f.onStop()
		}
	case "delete":
		if f.inst == nil {
			return errors.New("fakeLimaCtl: delete: instance missing")
		}
		f.inst = nil
	}
	return nil
}

func (f *fakeLimaCtl) verbs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.args[0]
	}
	return out
}

func (f *fakeLimaCtl) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// setStatus changes the instance status without a call, as a host reboot would.
func (f *fakeLimaCtl) setStatus(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inst.Status = s
}

func (f *fakeLimaCtl) status() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inst == nil {
		return ""
	}
	return f.inst.Status
}

type sshCall struct {
	target SSHTarget
	remote string
	stdin  string
}

// fakeGuest mirrors the guest files the manager writes and the boot script
// reads: ModeFile, PackagesFile, ProvisionedMarker, plus whether the
// omac-vmguard table is loaded and which images Docker holds. bootDone is
// /run/lima-boot-done: set once a boot's provisioning finished, cleared by
// a shutdown.
type fakeGuest struct {
	modeFile    string
	packages    string
	provisioned bool
	firewall    bool
	bootDone    bool
	images      []string
}

type fakeSSH struct {
	mu    sync.Mutex
	calls []sshCall
	guest fakeGuest
	// stickyFirewall keeps the table loaded across boots, simulating a guest
	// that ignores the online mode.
	stickyFirewall bool
	// provisionFailures makes that many boots leave ProvisionedMarker absent,
	// as a failed system provisioning script would.
	provisionFailures int
	// slowBoots makes that many boots leave provisioning running: the
	// marker and bootDone stay unset until the next successful Docker probe
	// (see finishPending).
	slowBoots int
	pending   bool
	fail      func(remote string) error
}

func (f *fakeSSH) run(_ context.Context, t SSHTarget, stdin io.Reader, stdout, _ io.Writer, remote string) error {
	var in string
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		in = string(b)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sshCall{t, remote, in})
	if f.fail != nil {
		if err := f.fail(remote); err != nil {
			return err
		}
	}
	g := &f.guest
	if i := strings.Index(remote, " > "+PackagesFile); i >= 0 {
		g.packages = strings.Trim(strings.TrimPrefix(remote[:i], `printf '%s\n' `), "'")
	}
	if strings.Contains(remote, "rm -f "+ProvisionedMarker) {
		g.provisioned = false
	}
	switch {
	case strings.Contains(remote, writeModeCmd(ModeOnline)):
		g.modeFile = ModeOnline
	case strings.Contains(remote, writeModeCmd(ModeOffline)):
		g.modeFile = ModeOffline
	}
	switch {
	case remote == verifyFirewallCmd:
		if !g.firewall {
			return errors.New("exit status 1: Error: No such file or directory")
		}
	case remote == markerPresentCmd:
		if !g.provisioned {
			return errors.New("exit status 1")
		}
	case remote == bootDoneCmd:
		if !g.bootDone {
			return errors.New("exit status 1")
		}
	case remote == dockerCmd("load"):
		g.images = append(g.images, in)
	case strings.HasPrefix(remote, dockerCmd("image ls")):
		for _, img := range g.images {
			fmt.Fprintln(stdout, img)
		}
	case strings.HasPrefix(remote, "if "+dockerCmd("image inspect ")):
		// imageRemoveCmd: removes the image if Docker has it, else exits 0.
		ref := strings.TrimPrefix(remote, "if "+dockerCmd("image inspect "))
		ref = strings.Trim(ref[:strings.Index(ref, " ")], "'")
		g.images = slices.DeleteFunc(g.images, func(s string) bool { return s == ref })
	case strings.HasPrefix(remote, dockerCmd("image rm ")):
		ref := strings.Trim(strings.TrimPrefix(remote, dockerCmd("image rm ")), "'")
		if !slices.Contains(g.images, ref) {
			return errors.New("exit status 1: Error response from daemon: No such image: " + ref)
		}
		g.images = slices.DeleteFunc(g.images, func(s string) bool { return s == ref })
	}
	return nil
}

// boot applies the boot script's mode logic: offline unless not yet
// provisioned, ModeFile wins; the first boot provisions.
func (f *fakeSSH) boot() {
	f.mu.Lock()
	defer f.mu.Unlock()
	mode := ModeOffline
	if !f.guest.provisioned {
		mode = ModeOnline
	}
	if f.guest.modeFile != "" {
		mode = f.guest.modeFile
	}
	f.guest.firewall = mode == ModeOffline || f.stickyFirewall
	if f.slowBoots > 0 {
		f.slowBoots--
		f.pending = true
		return
	}
	f.finishBoot()
}

// finishBoot ends the provisioning of the current boot. Called with f.mu held.
func (f *fakeSSH) finishBoot() {
	f.guest.bootDone = true
	if f.provisionFailures > 0 {
		f.provisionFailures--
		return
	}
	f.guest.provisioned = true
}

// finishPending completes a slow boot; Docker answering means the
// provisioning scripts are done.
func (f *fakeSSH) finishPending() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending {
		f.pending = false
		f.finishBoot()
	}
}

func (f *fakeSSH) halt() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.guest.firewall = false
	f.guest.bootDone = false
	f.pending = false
}

func (f *fakeSSH) snapshot() ([]sshCall, fakeGuest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.guest
	g.images = slices.Clone(g.images)
	return slices.Clone(f.calls), g
}

func (f *fakeSSH) setFirewall(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.guest.firewall = on
}

func (f *fakeSSH) remotes() []string {
	calls, _ := f.snapshot()
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.remote
	}
	return out
}

// fakeProbe answers like Docker while the fake instance runs; err forces
// a failure.
type fakeProbe struct {
	mu      sync.Mutex
	calls   int
	err     error
	lima    *fakeLimaCtl
	onReady func() // runs after each successful probe
}

func (p *fakeProbe) run(_ context.Context, _ int) (DockerVersion, error) {
	p.mu.Lock()
	p.calls++
	forced := p.err
	p.mu.Unlock()
	if forced != nil {
		return DockerVersion{}, forced
	}
	if p.lima.status() != StatusRunning {
		return DockerVersion{}, errors.New("connection refused")
	}
	if p.onReady != nil {
		p.onReady()
	}
	return DockerVersion{APIVersion: "1.47", Version: "27.3.1"}, nil
}

func (p *fakeProbe) setErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func newTestManager(t *testing.T) (*Manager, *fakeLimaCtl, *fakeSSH, *fakeProbe) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "vm")
	lima := &fakeLimaCtl{failOn: map[string]error{}}
	ssh := &fakeSSH{}
	lima.onStart = ssh.boot
	lima.onStop = ssh.halt
	probe := &fakeProbe{lima: lima, onReady: ssh.finishPending}
	n := 0
	keygen := func(_ context.Context, name string, args []string, _ io.Reader, _, _ io.Writer) error {
		i := slices.Index(args, "-f")
		if name != "ssh-keygen" || i < 0 || i+1 >= len(args) {
			return fmt.Errorf("unexpected command %s %v", name, args)
		}
		n++
		path := args[i+1]
		if err := os.WriteFile(path, fmt.Appendf(nil, "private-%d\n", n), 0o600); err != nil {
			return err
		}
		return os.WriteFile(path+".pub", fmt.Appendf(nil, "ssh-ed25519 AAAAkey%d test\n", n), 0o644)
	}
	deps := Deps{
		Lima:  lima.run,
		SSH:   ssh.run,
		Run:   keygen,
		Probe: probe.run,
		Pull: func(_ context.Context, ref, tar string) error {
			return os.WriteFile(tar, []byte(ref), 0o600)
		},
		LookPath:         func(string) (string, error) { return "/usr/bin/limactl", nil },
		PortFree:         func(int) bool { return true },
		Host:             HostResources{CPUs: 16, MemoryBytes: 64 << 30},
		Alive:            func(int) bool { return false },
		HostUser:         "alice",
		Now:              time.Now,
		ProbeInterval:    time.Millisecond,
		ProvisionTimeout: time.Second,
		RestartTimeout:   time.Second,
	}
	return NewManager(NewLayout(root), filepath.Join(t.TempDir(), "vm.yaml"), deps, io.Discard), lima, ssh, probe
}
