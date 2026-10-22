package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func mustUp(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Up(context.Background(), false); err != nil {
		t.Fatalf("Up: %v", err)
	}
}

func mustState(t *testing.T, m *Manager) *State {
	t.Helper()
	st, err := LoadState(m.Layout.StatePath)
	if err != nil || st == nil {
		t.Fatalf("LoadState = %v, %v", st, err)
	}
	return st
}

func assertVerbs(t *testing.T, lima *fakeLimaCtl, want ...string) {
	t.Helper()
	if got := lima.verbs(); !slices.Equal(got, want) {
		t.Errorf("limactl verbs = %v, want %v", got, want)
	}
}

func writeConfig(t *testing.T, m *Manager, content string) {
	t.Helper()
	if err := os.WriteFile(m.ConfigPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUpCreatesProvisionsAndGoesOffline(t *testing.T) {
	m, lima, ssh, _ := newTestManager(t)
	l := m.Layout
	mustUp(t, m)

	assertVerbs(t, lima, "list", "create", "start", "stop", "start")
	for _, c := range lima.calls {
		if c.home != l.LimaHome {
			t.Errorf("limactl %v ran with LIMA_HOME %q, want %q", c.args, c.home, l.LimaHome)
		}
	}
	if want := []string{"create", "--tty=false", "--name", InstanceName, l.LimaYAML}; !slices.Equal(lima.calls[1].args, want) {
		t.Errorf("create args = %v, want %v", lima.calls[1].args, want)
	}
	if want := []string{"start", "--tty=false", InstanceName}; !slices.Equal(lima.calls[2].args, want) {
		t.Errorf("start args = %v, want %v", lima.calls[2].args, want)
	}

	yaml, err := os.ReadFile(l.LimaYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"localPort: 42022", "hostPort: 42375", "name: alice", "AAAAkey1", "AAAAkey2"} {
		if !strings.Contains(string(yaml), want) {
			t.Errorf("lima yaml lacks %q", want)
		}
	}
	for path, want := range map[string]os.FileMode{l.Root: 0o700, l.LimaHome: 0o700, l.LimaYAML: 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v", path, fi.Mode().Perm(), want)
		}
	}
	for _, k := range []string{l.AdminKey, l.AdminKey + ".pub", l.AgentKey, l.AgentKey + ".pub"} {
		if _, err := os.Stat(k); err != nil {
			t.Errorf("key file: %v", err)
		}
	}

	st := mustState(t, m)
	if st.Mode != ModeOffline || st.SSHPort != 42022 || st.DockerPort != 42375 {
		t.Errorf("state = %+v, want offline on 42022/42375", st)
	}

	calls, guest := ssh.snapshot()
	wantRoot := SSHTarget{User: "root", Port: 42022, KeyPath: l.AdminKey, KnownHosts: l.KnownHosts}
	var wroteMode, verified bool
	for _, c := range calls {
		if c.target != wantRoot {
			t.Errorf("ssh %q ran as %+v, want %+v", c.remote, c.target, wantRoot)
		}
		wroteMode = wroteMode || c.remote == "printf '%s' offline > /etc/omac-vm/mode"
		verified = verified || c.remote == "nft list table inet omac-vmguard"
	}
	if !wroteMode || !verified {
		t.Errorf("ssh calls %v lack the offline write or the nft check", ssh.remotes())
	}
	if !guest.firewall || guest.modeFile != ModeOffline {
		t.Errorf("guest = %+v, want firewall loaded and mode offline", guest)
	}
}

func TestUpIsIdempotentWhenRunningOffline(t *testing.T) {
	m, lima, ssh, _ := newTestManager(t)
	mustUp(t, m)
	lima.reset()
	before := len(ssh.remotes())
	var out bytes.Buffer
	m.Out = &out

	mustUp(t, m)
	assertVerbs(t, lima, "list")
	if n := len(ssh.remotes()); n != before {
		t.Errorf("second Up ran %d ssh commands, want none", n-before)
	}
	if !strings.Contains(out.String(), "status: running") {
		t.Errorf("second Up output = %q, want a status report", out.String())
	}
}

func TestUpStartsStoppedInstanceOffline(t *testing.T) {
	m, lima, ssh, _ := newTestManager(t)
	mustUp(t, m)
	lima.setStatus(StatusStopped) // host rebooted
	ssh.halt()
	lima.reset()

	mustUp(t, m)
	assertVerbs(t, lima, "list", "start", "stop", "start")
	if _, g := ssh.snapshot(); !g.firewall {
		t.Error("firewall not loaded after Up of a stopped instance")
	}
	if remotes := ssh.remotes(); remotes[len(remotes)-1] != verifyFirewallCmd {
		t.Errorf("last ssh command = %q, want the firewall check", remotes[len(remotes)-1])
	}
	if st := mustState(t, m); st.Mode != ModeOffline {
		t.Errorf("mode = %q, want offline", st.Mode)
	}
}

func TestUpFailsWhenFirewallMissingOffline(t *testing.T) {
	m, _, ssh, _ := newTestManager(t)
	ssh.fail = func(remote string) error {
		if remote == verifyFirewallCmd {
			return errors.New("exit status 1")
		}
		return nil
	}
	err := m.Up(context.Background(), false)
	if !errors.Is(err, ErrFirewallMissing) || !strings.Contains(err.Error(), "omac-vmguard") {
		t.Fatalf("Up = %v, want ErrFirewallMissing naming omac-vmguard", err)
	}
	if st := mustState(t, m); st.Mode != ModeOnline {
		t.Errorf("mode = %q after failed verification, want online", st.Mode)
	}
	if _, err := m.PrepareAttach(context.Background()); !errors.Is(err, ErrOnline) {
		t.Errorf("PrepareAttach = %v, want ErrOnline", err)
	}
}

func TestUpMissingLimactl(t *testing.T) {
	m, lima, _, _ := newTestManager(t)
	m.Deps.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if err := m.Up(context.Background(), false); !errors.Is(err, ErrLimaMissing) {
		t.Fatalf("Up = %v, want ErrLimaMissing", err)
	}
	assertVerbs(t, lima)
}

func TestUpReprovisionRewritesPackagesAndCycles(t *testing.T) {
	m, lima, ssh, _ := newTestManager(t)
	mustUp(t, m)
	writeConfig(t, m, "packages: [jq]\n")
	lima.reset()

	if err := m.Up(context.Background(), true); err != nil {
		t.Fatalf("Up(reprovision): %v", err)
	}
	assertVerbs(t, lima, "list", "stop", "start", "stop", "start")
	remotes := strings.Join(ssh.remotes(), "\n")
	for _, want := range []string{"> /etc/omac-vm/packages", "rm -f /var/lib/omac-vm/provisioned"} {
		if !strings.Contains(remotes, want) {
			t.Errorf("ssh commands lack %q:\n%s", want, remotes)
		}
	}
	if _, g := ssh.snapshot(); g.packages != "jq" || !g.firewall || !g.provisioned {
		t.Errorf("guest = %+v, want packages jq, provisioned, firewall loaded", g)
	}
	st := mustState(t, m)
	if !slices.Equal(st.Packages, []string{"jq"}) || st.Mode != ModeOffline {
		t.Errorf("state = %+v, want packages [jq], offline", st)
	}
}

func TestUpReprovisionStartsStoppedInstanceFirst(t *testing.T) {
	m, lima, _, _ := newTestManager(t)
	mustUp(t, m)
	if err := m.Down(context.Background()); err != nil {
		t.Fatal(err)
	}
	lima.reset()
	if err := m.Up(context.Background(), true); err != nil {
		t.Fatalf("Up(reprovision): %v", err)
	}
	assertVerbs(t, lima, "list", "start", "stop", "start", "stop", "start")
}

func TestUpReprovisionRejectsBadPackageName(t *testing.T) {
	m, lima, _, _ := newTestManager(t)
	mustUp(t, m)
	writeConfig(t, m, "packages: [\"jq; reboot\"]\n")
	lima.reset()
	if err := m.Up(context.Background(), true); err == nil || !strings.HasPrefix(err.Error(), "omac vm:") {
		t.Fatalf("Up = %v, want an omac vm: error", err)
	}
	assertVerbs(t, lima, "list")
}

func TestDownStopsAndKeepsState(t *testing.T) {
	m, lima, _, _ := newTestManager(t)
	mustUp(t, m)
	lima.reset()
	if err := m.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}
	assertVerbs(t, lima, "list", "stop")
	if lima.status() != StatusStopped {
		t.Errorf("status = %q, want stopped", lima.status())
	}
	if st := mustState(t, m); st.Mode != ModeOffline {
		t.Errorf("state mode = %q, want offline kept", st.Mode)
	}
	lima.reset()
	if err := m.Down(context.Background()); err != nil {
		t.Fatalf("second Down: %v", err)
	}
	assertVerbs(t, lima, "list")
}

func TestDownNotCreated(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	if err := m.Down(context.Background()); !errors.Is(err, ErrNotCreated) {
		t.Fatalf("Down = %v, want ErrNotCreated", err)
	}
}

func TestResetDeletesEverythingThenUp(t *testing.T) {
	m, lima, _, _ := newTestManager(t)
	l := m.Layout
	mustUp(t, m)
	oldKey, _ := os.ReadFile(l.AdminKey)
	st := mustState(t, m)
	st.Images = []string{"index.docker.io/library/alpine:latest"}
	if err := SaveState(l.StatePath, st); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterSession(l, 999999); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(l.Root, "images", "left.tar")
	if err := os.MkdirAll(filepath.Dir(stray), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lima.reset()

	if err := m.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	assertVerbs(t, lima, "list", "delete", "list", "create", "start", "stop", "start")
	if want := []string{"delete", "-f", InstanceName}; !slices.Equal(lima.calls[1].args, want) {
		t.Errorf("delete args = %v, want %v", lima.calls[1].args, want)
	}
	if newKey, _ := os.ReadFile(l.AdminKey); bytes.Equal(newKey, oldKey) {
		t.Error("admin key unchanged after reset")
	}
	if st := mustState(t, m); len(st.Images) != 0 || st.Mode != ModeOffline {
		t.Errorf("state after reset = %+v, want fresh and offline", st)
	}
	for _, p := range []string{l.SessionsDir, filepath.Dir(stray)} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived reset (err %v)", p, err)
		}
	}
	if lima.status() != StatusRunning {
		t.Errorf("status = %q, want running", lima.status())
	}
}

func TestResetWithoutInstanceSkipsDelete(t *testing.T) {
	m, lima, _, _ := newTestManager(t)
	if err := m.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if slices.Contains(lima.verbs(), "delete") {
		t.Errorf("verbs = %v, want no delete", lima.verbs())
	}
}

func TestSetModeOnlineRefusesWithSessions(t *testing.T) {
	m, lima, _, _ := newTestManager(t)
	mustUp(t, m)
	if _, err := RegisterSession(m.Layout, 4242); err != nil {
		t.Fatal(err)
	}
	m.Deps.Alive = func(int) bool { return true }
	lima.reset()

	err := m.SetMode(context.Background(), ModeOnline)
	if !errors.Is(err, ErrSessionsAttached) || !strings.Contains(err.Error(), "4242") {
		t.Fatalf("SetMode(online) = %v, want ErrSessionsAttached naming 4242", err)
	}
	assertVerbs(t, lima)
}

func TestSetModeOnlineCyclesAndVerifiesNoFirewall(t *testing.T) {
	m, lima, ssh, _ := newTestManager(t)
	mustUp(t, m)
	lima.reset()
	nBefore := len(ssh.remotes())

	if err := m.SetMode(context.Background(), ModeOnline); err != nil {
		t.Fatalf("SetMode(online): %v", err)
	}
	assertVerbs(t, lima, "list", "stop", "start")
	if _, g := ssh.snapshot(); g.modeFile != ModeOnline || g.firewall {
		t.Errorf("guest = %+v, want mode online and no firewall", g)
	}
	if !slices.Contains(ssh.remotes()[nBefore:], verifyFirewallCmd) {
		t.Error("online switch did not run the firewall check")
	}
	if st := mustState(t, m); st.Mode != ModeOnline {
		t.Errorf("mode = %q, want online", st.Mode)
	}
	if _, err := m.PrepareAttach(context.Background()); !errors.Is(err, ErrOnline) {
		t.Errorf("PrepareAttach = %v, want ErrOnline", err)
	}
}

func TestSetModeOnlineFailsWhenFirewallStillLoaded(t *testing.T) {
	m, _, ssh, _ := newTestManager(t)
	mustUp(t, m)
	ssh.mu.Lock()
	ssh.stickyFirewall = true
	ssh.mu.Unlock()
	err := m.SetMode(context.Background(), ModeOnline)
	if err == nil || !strings.Contains(err.Error(), "still loaded") {
		t.Fatalf("SetMode(online) = %v, want a still-loaded error", err)
	}
}

func TestSetModeOfflineRestoresFirewall(t *testing.T) {
	m, _, ssh, _ := newTestManager(t)
	mustUp(t, m)
	if err := m.SetMode(context.Background(), ModeOnline); err != nil {
		t.Fatal(err)
	}
	if err := m.SetMode(context.Background(), ModeOffline); err != nil {
		t.Fatalf("SetMode(offline): %v", err)
	}
	if _, g := ssh.snapshot(); g.modeFile != ModeOffline || !g.firewall {
		t.Errorf("guest = %+v, want offline with firewall", g)
	}
	if st := mustState(t, m); st.Mode != ModeOffline {
		t.Errorf("mode = %q, want offline", st.Mode)
	}
	if _, err := m.PrepareAttach(context.Background()); err != nil {
		t.Errorf("PrepareAttach after offline: %v", err)
	}
}

func TestSetModeRejectsUnknownMode(t *testing.T) {
	m, lima, _, _ := newTestManager(t)
	if err := m.SetMode(context.Background(), "airplane"); err == nil {
		t.Fatal("SetMode(airplane) succeeded")
	}
	assertVerbs(t, lima)
}

func TestStatusReportsEverything(t *testing.T) {
	m, _, _, _ := newTestManager(t)
	ctx := context.Background()
	r, err := m.Status(ctx)
	if err != nil || r.Created {
		t.Fatalf("Status before Up = %+v, %v; want not created", r, err)
	}
	mustUp(t, m)
	if err := m.ImageAdd(ctx, "alpine"); err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterSession(m.Layout, 77); err != nil {
		t.Fatal(err)
	}
	m.Deps.Alive = func(pid int) bool { return pid == 77 }

	r, err = m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	want := StatusReport{
		Created: true, Running: true, Mode: ModeOffline, SSHPort: 42022, DockerPort: 42375,
		Resources: Resources{CPUs: 8, Memory: "16GiB", Disk: "60GiB"},
		Images:    []string{"index.docker.io/library/alpine:latest"},
		Sessions:  []int{77},
	}
	if r.Docker == nil || r.Docker.APIVersion != "1.47" {
		t.Errorf("Docker = %+v, want API 1.47", r.Docker)
	}
	r.Docker = nil
	if fmt.Sprintf("%+v", r) != fmt.Sprintf("%+v", want) {
		t.Errorf("Status =\n%+v\nwant\n%+v", r, want)
	}
}

func TestStatusToleratesProbeFailure(t *testing.T) {
	m, _, _, probe := newTestManager(t)
	mustUp(t, m)
	probe.setErr(errors.New("connection refused"))
	r, err := m.Status(context.Background())
	if err != nil || r.Docker != nil || !r.Running {
		t.Fatalf("Status = %+v, %v; want running with Docker nil", r, err)
	}
}

func TestImageAddPullsLoadsAndRecords(t *testing.T) {
	m, _, ssh, _ := newTestManager(t)
	ctx := context.Background()
	mustUp(t, m)
	var pulls []string
	pull := m.Deps.Pull
	m.Deps.Pull = func(ctx context.Context, ref, tar string) error {
		pulls = append(pulls, ref)
		if filepath.Dir(tar) != filepath.Join(m.Layout.Root, "images") {
			t.Errorf("tar path %s outside <Root>/images", tar)
		}
		return pull(ctx, ref, tar)
	}

	if err := m.ImageAdd(ctx, "alpine"); err != nil {
		t.Fatalf("ImageAdd: %v", err)
	}
	const norm = "index.docker.io/library/alpine:latest"
	if !slices.Equal(pulls, []string{norm}) {
		t.Errorf("pulls = %v, want [%s]", pulls, norm)
	}
	calls, guest := ssh.snapshot()
	last := calls[len(calls)-1]
	wantAgent := SSHTarget{User: "alice", Port: 42022, KeyPath: m.Layout.AgentKey, KnownHosts: m.Layout.KnownHosts}
	if last.remote != dockerCmd("load") || last.stdin != norm || last.target != wantAgent {
		t.Errorf("last ssh = %+v, want docker load of the tar as agent", last)
	}
	if !slices.Equal(guest.images, []string{norm}) {
		t.Errorf("guest images = %v", guest.images)
	}
	if st := mustState(t, m); !slices.Equal(st.Images, []string{norm}) {
		t.Errorf("state images = %v, want [%s]", st.Images, norm)
	}
	if entries, _ := os.ReadDir(filepath.Join(m.Layout.Root, "images")); len(entries) != 0 {
		t.Errorf("tar left behind: %v", entries)
	}
	if fi, err := os.Stat(filepath.Join(m.Layout.Root, "images")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("images dir = %v, %v; want mode 0700", fi, err)
	}

	if err := m.ImageAdd(ctx, "alpine:latest"); err != nil {
		t.Fatal(err)
	}
	if st := mustState(t, m); len(st.Images) != 1 {
		t.Errorf("state images = %v, want deduplicated", st.Images)
	}

	if err := m.Down(ctx); err != nil {
		t.Fatal(err)
	}
	pulls = nil
	if err := m.ImageAdd(ctx, "busybox"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("ImageAdd on stopped VM = %v, want ErrNotRunning", err)
	}
	if len(pulls) != 0 {
		t.Errorf("pulled %v on a stopped VM", pulls)
	}
}

func TestImageAddInvalidRef(t *testing.T) {
	m, lima, _, _ := newTestManager(t)
	err := m.ImageAdd(context.Background(), "not a ref!")
	if err == nil || !strings.HasPrefix(err.Error(), "omac vm:") {
		t.Fatalf("ImageAdd = %v, want omac vm: error", err)
	}
	assertVerbs(t, lima)
}

func TestImageListMergesStateAndVM(t *testing.T) {
	m, _, ssh, _ := newTestManager(t)
	ctx := context.Background()
	mustUp(t, m)
	if err := m.ImageAdd(ctx, "alpine"); err != nil {
		t.Fatal(err)
	}
	ssh.mu.Lock()
	ssh.guest.images = append(ssh.guest.images, "busybox:1.36", "<none>:<none>")
	ssh.mu.Unlock()

	got, err := m.ImageList(ctx)
	if err != nil {
		t.Fatalf("ImageList: %v", err)
	}
	want := []string{"index.docker.io/library/alpine:latest", "index.docker.io/library/busybox:1.36"}
	if !slices.Equal(got, want) {
		t.Errorf("ImageList = %v, want %v", got, want)
	}
}

func TestImageRemove(t *testing.T) {
	m, _, ssh, _ := newTestManager(t)
	ctx := context.Background()
	mustUp(t, m)
	if err := m.ImageAdd(ctx, "alpine"); err != nil {
		t.Fatal(err)
	}
	if err := m.ImageRemove(ctx, "alpine"); err != nil {
		t.Fatalf("ImageRemove: %v", err)
	}
	if !slices.Contains(ssh.remotes(), dockerCmd("image rm 'index.docker.io/library/alpine:latest'")) {
		t.Errorf("no docker image rm in %v", ssh.remotes())
	}
	if _, g := ssh.snapshot(); len(g.images) != 0 {
		t.Errorf("guest images = %v, want none", g.images)
	}
	if st := mustState(t, m); len(st.Images) != 0 {
		t.Errorf("state images = %v, want none", st.Images)
	}
}

func TestPrepareAttachChecks(t *testing.T) {
	m, _, _, probe := newTestManager(t)
	ctx := context.Background()
	if _, err := m.PrepareAttach(ctx); !errors.Is(err, ErrNotCreated) {
		t.Fatalf("not created: %v", err)
	}
	mustUp(t, m)
	if err := m.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PrepareAttach(ctx); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped: %v", err)
	}
	mustUp(t, m)
	if err := m.SetMode(ctx, ModeOnline); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PrepareAttach(ctx); !errors.Is(err, ErrOnline) {
		t.Fatalf("online: %v", err)
	}
	if err := m.SetMode(ctx, ModeOffline); err != nil {
		t.Fatal(err)
	}
	probe.setErr(fmt.Errorf("port 42375: %w", ErrNotDocker))
	if _, err := m.PrepareAttach(ctx); !errors.Is(err, ErrNotDocker) {
		t.Fatalf("not docker: %v", err)
	}
	probe.setErr(nil)

	info, err := m.PrepareAttach(ctx)
	if err != nil {
		t.Fatalf("PrepareAttach: %v", err)
	}
	want := AttachInfo{DockerPort: 42375, SSHPort: 42022, AgentKeyPath: m.Layout.AgentKey, User: "alice",
		Docker: DockerVersion{APIVersion: "1.47", Version: "27.3.1"}}
	if info != want {
		t.Errorf("AttachInfo = %+v, want %+v", info, want)
	}
	if got := info.DockerHost(); got != "tcp://127.0.0.1:42375" {
		t.Errorf("DockerHost = %q", got)
	}
}

func TestPrepareAttachRequiresLoadedFirewall(t *testing.T) {
	m, _, ssh, probe := newTestManager(t)
	ctx := context.Background()
	mustUp(t, m)
	ssh.setFirewall(false) // state says offline, but the table is gone
	probeCalls := probe.calls

	_, err := m.PrepareAttach(ctx)
	if !errors.Is(err, ErrFirewallMissing) {
		t.Fatalf("PrepareAttach = %v, want ErrFirewallMissing", err)
	}
	if probe.calls != probeCalls {
		t.Error("probed Docker although the firewall check failed")
	}
	ssh.fail = func(string) error { return errors.New("ssh: connect refused") }
	ssh.setFirewall(true)
	if _, err := m.PrepareAttach(ctx); !errors.Is(err, ErrFirewallMissing) {
		t.Fatalf("PrepareAttach with failing ssh = %v, want ErrFirewallMissing", err)
	}
}

func TestStatusReportWrite(t *testing.T) {
	r := StatusReport{
		Created: true, Running: true, Mode: ModeOffline, SSHPort: 42022, DockerPort: 42375,
		Resources: Resources{CPUs: 8, Memory: "16GiB", Disk: "60GiB"},
		Images:    []string{"index.docker.io/library/alpine:latest", "index.docker.io/library/busybox:1.36"},
		Docker:    &DockerVersion{APIVersion: "1.47", Version: "27.3.1"},
	}
	var b bytes.Buffer
	r.Write(&b)
	out := b.String()
	for _, want := range []string{
		"status: running", "mode: offline", "ssh: 127.0.0.1:42022", "docker: tcp://127.0.0.1:42375",
		"images:\n  - index.docker.io/library/alpine:latest\n  - index.docker.io/library/busybox:1.36\n",
		"sessions: none",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}

	b.Reset()
	StatusReport{}.Write(&b)
	if !strings.Contains(b.String(), "status: not created") {
		t.Errorf("empty report = %q", b.String())
	}
}
