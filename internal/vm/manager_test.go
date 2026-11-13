package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
	m, lima, ssh := newTestManager(t)
	m.Deps.ProvisionTimeout = 2 * time.Second
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
	// The provisioning boot gets the provisioning timeout, the restart into
	// offline mode the restart timeout.
	if want := []string{"start", "--tty=false", "--timeout", "2s", InstanceName}; !slices.Equal(lima.calls[2].args, want) {
		t.Errorf("first start args = %v, want %v", lima.calls[2].args, want)
	}
	if want := []string{"start", "--tty=false", "--timeout", "1s", InstanceName}; !slices.Equal(lima.calls[4].args, want) {
		t.Errorf("second start args = %v, want %v", lima.calls[4].args, want)
	}

	yaml, err := os.ReadFile(l.LimaYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"localPort: 42022", "name: alice", "AAAAkey1", "AAAAkey2"} {
		if !strings.Contains(string(yaml), want) {
			t.Errorf("lima yaml lacks %q", want)
		}
	}
	if strings.Contains(string(yaml), "42375") {
		t.Error("lima yaml forwards a Docker port")
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
	if st.Mode != ModeOffline || st.SSHPort != 42022 {
		t.Errorf("state = %+v, want offline on 42022", st)
	}

	calls, guest := ssh.snapshot()
	wantRoot := SSHTarget{User: "root", Port: 42022, KeyPath: l.AdminKey, KnownHosts: l.KnownHosts}
	// The Docker check runs as the agent user, who owns rootless dockerd.
	wantAgent := SSHTarget{User: "alice", Port: 42022, KeyPath: l.AgentKey, KnownHosts: l.KnownHosts}
	var wroteMode, verified bool
	for _, c := range calls {
		want := wantRoot
		if c.remote == dockerVersionCmd {
			want = wantAgent
		}
		if c.target != want {
			t.Errorf("ssh %q ran as %+v, want %+v", c.remote, c.target, want)
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
	m, lima, ssh := newTestManager(t)
	mustUp(t, m)
	lima.reset()
	before := len(ssh.remotes())
	var out bytes.Buffer
	m.Out = &out

	mustUp(t, m)
	assertVerbs(t, lima, "list")
	// Only the status report's Docker check over ssh.
	if got := ssh.remotes()[before:]; !slices.Equal(got, []string{dockerVersionCmd}) {
		t.Errorf("second Up ran ssh commands %q, want only the Docker check", got)
	}
	if !strings.Contains(out.String(), "status: running") {
		t.Errorf("second Up output = %q, want a status report", out.String())
	}
}

func TestUpStartsStoppedInstanceOffline(t *testing.T) {
	m, lima, ssh := newTestManager(t)
	mustUp(t, m)
	lima.setStatus(StatusStopped) // host rebooted
	ssh.halt()
	lima.reset()

	mustUp(t, m)
	assertVerbs(t, lima, "list", "start")
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

func TestUpStoppedInstanceFallsBackWhenFirewallMissing(t *testing.T) {
	m, lima, ssh := newTestManager(t)
	mustUp(t, m)
	lima.setStatus(StatusStopped)
	ssh.mu.Lock()
	ssh.guest.modeFile = ModeOnline // guest disagrees with state
	ssh.guest.firewall = false
	ssh.mu.Unlock()
	lima.reset()

	mustUp(t, m)
	assertVerbs(t, lima, "list", "start", "stop", "start")
	if _, g := ssh.snapshot(); g.modeFile != ModeOffline || !g.firewall {
		t.Errorf("guest = %+v, want mode offline with firewall", g)
	}
	if st := mustState(t, m); st.Mode != ModeOffline {
		t.Errorf("mode = %q, want offline", st.Mode)
	}
}

func TestUpRecoversFromInterruptedProvisioning(t *testing.T) {
	m, lima, ssh := newTestManager(t)
	lima.failOn["start"] = errors.New("boot timed out")

	err := m.Up(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "run `omac vm up` again (or `omac vm up --reprovision`)") {
		t.Fatalf("Up = %v, want the retry hint", err)
	}
	if st := mustState(t, m); st.Mode != ModeOnline {
		t.Errorf("mode after interrupted create = %q, want online", st.Mode)
	}

	delete(lima.failOn, "start")
	ssh.mu.Lock()
	ssh.provisionFailures = 1 // the next boot's provisioning fails too
	ssh.mu.Unlock()
	lima.reset()
	var out bytes.Buffer
	m.Out = &out
	mustUp(t, m)
	if !strings.Contains(out.String(), "provisioning was not completed, re-running it") {
		t.Errorf("output = %q, want the re-run notice", out.String())
	}
	assertVerbs(t, lima, "list", "start", "stop", "start", "stop", "start")
	remotes := ssh.remotes()
	if !slices.Contains(remotes, markerPresentCmd) || !slices.Contains(remotes, reprovisionCmd(nil)) {
		t.Errorf("ssh commands %v lack the marker check or the reprovision command", remotes)
	}
	if _, g := ssh.snapshot(); !g.provisioned || !g.firewall || g.modeFile != ModeOffline {
		t.Errorf("guest = %+v, want provisioned, offline, firewall loaded", g)
	}
	if st := mustState(t, m); st.Mode != ModeOffline {
		t.Errorf("mode = %q, want offline", st.Mode)
	}
}

// limactl start times out while the guest still runs apt. The next up must
// refuse instead of stopping the VM in the middle of that provisioning.
func TestUpRefusesWhileProvisioningStillRunning(t *testing.T) {
	m, lima, ssh := newTestManager(t)
	ssh.slowBoots = 1
	lima.startTimeout = errors.New(`did not receive an event with the "running" status`)
	if err := m.Up(context.Background(), false); err == nil || !strings.Contains(err.Error(), retryUpHint) {
		t.Fatalf("Up = %v, want the retry hint", err)
	}
	if _, g := ssh.snapshot(); g.provisioned || g.bootDone || lima.status() != StatusRunning {
		t.Fatalf("setup: guest %+v, status %q; want running and still provisioning", g, lima.status())
	}
	lima.reset()
	nSSH := len(ssh.remotes())

	err := m.Up(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "provisioning is still running") {
		t.Fatalf("Up = %v, want the still-running refusal", err)
	}
	assertVerbs(t, lima, "list")
	if ssh.stoppedWhileProvisioning() {
		t.Error("the VM was stopped while provisioning still ran")
	}
	if remotes := ssh.remotes()[nSSH:]; !slices.Equal(remotes, []string{bootDoneCmd}) {
		t.Errorf("ssh commands = %v, want only the boot-done check", remotes)
	}
	if st := mustState(t, m); st.Mode != ModeOnline {
		t.Errorf("mode = %q, want online kept", st.Mode)
	}
}

// A stopped VM whose state says online may lack the provisioned marker, so
// its boot provisions: it needs the provisioning timeout and the retry hint.
func TestUpStartsStoppedOnlineWithProvisionTimeout(t *testing.T) {
	m, lima, _ := newTestManager(t)
	ctx := context.Background()
	mustUp(t, m)
	if err := m.SetMode(ctx, ModeOnline); err != nil {
		t.Fatal(err)
	}
	if err := m.Down(ctx); err != nil {
		t.Fatal(err)
	}
	m.Deps.ProvisionTimeout = 3 * time.Second
	lima.reset()
	lima.failOn["start"] = errors.New("boot failed")

	err := m.Up(ctx, false)
	if err == nil || !strings.Contains(err.Error(), retryUpHint) {
		t.Fatalf("Up = %v, want the retry hint", err)
	}
	want := []string{"start", "--tty=false", "--timeout", "3s", InstanceName}
	if len(lima.calls) != 2 || !slices.Equal(lima.calls[1].args, want) {
		t.Errorf("limactl calls = %v, want list then %v", lima.calls, want)
	}
}

func TestUpRerunsProvisioningWhenRunningUnprovisioned(t *testing.T) {
	m, lima, ssh := newTestManager(t)
	ssh.provisionFailures = 1
	ssh.fail = dockerDown // Docker never comes up
	if err := m.Up(context.Background(), false); err == nil || !strings.Contains(err.Error(), "omac vm up --reprovision") {
		t.Fatalf("Up = %v, want the retry hint", err)
	}
	if _, g := ssh.snapshot(); g.provisioned || lima.status() != StatusRunning {
		t.Fatalf("setup: guest %+v, status %q; want running and unprovisioned", g, lima.status())
	}
	ssh.fail = nil
	lima.reset()

	mustUp(t, m)
	assertVerbs(t, lima, "list", "stop", "start", "stop", "start")
	if !slices.Contains(ssh.remotes(), reprovisionCmd(nil)) {
		t.Errorf("ssh commands %v lack the reprovision command", ssh.remotes())
	}
	if _, g := ssh.snapshot(); !g.provisioned || !g.firewall || g.modeFile != ModeOffline {
		t.Errorf("guest = %+v, want provisioned, offline with firewall", g)
	}
}

func TestUpLeavesDeliberateOnlineAlone(t *testing.T) {
	m, lima, ssh := newTestManager(t)
	mustUp(t, m)
	if err := m.SetMode(context.Background(), ModeOnline); err != nil {
		t.Fatal(err)
	}
	lima.reset()
	nBefore := len(ssh.remotes())
	var out bytes.Buffer
	m.Out = &out

	mustUp(t, m)
	assertVerbs(t, lima, "list")
	if got := ssh.remotes()[nBefore:]; !slices.Equal(got, []string{bootDoneCmd, markerPresentCmd}) {
		t.Errorf("ssh commands = %v, want only the boot-done and marker checks", got)
	}
	if !strings.Contains(out.String(), "VM is online; run `omac vm offline`") {
		t.Errorf("output = %q, want the online hint", out.String())
	}
	if st := mustState(t, m); st.Mode != ModeOnline {
		t.Errorf("mode = %q, want online kept", st.Mode)
	}
}

func TestUpStartsStoppedDeliberateOnlineWithoutReprovision(t *testing.T) {
	m, lima, ssh := newTestManager(t)
	ctx := context.Background()
	mustUp(t, m)
	if err := m.SetMode(ctx, ModeOnline); err != nil {
		t.Fatal(err)
	}
	if err := m.Down(ctx); err != nil {
		t.Fatal(err)
	}
	lima.reset()

	mustUp(t, m)
	assertVerbs(t, lima, "list", "start")
	if slices.Contains(ssh.remotes(), reprovisionCmd(nil)) {
		t.Error("deliberate online VM was reprovisioned")
	}
	if _, g := ssh.snapshot(); g.modeFile != ModeOnline || g.firewall {
		t.Errorf("guest = %+v, want online without firewall", g)
	}
}

func TestReprovisionKeepsPackagesWhenOfflineSwitchFails(t *testing.T) {
	m, _, ssh := newTestManager(t)
	mustUp(t, m)
	writeConfig(t, m, "packages: [jq]\n")
	// The online check expects nft to fail; the final offline check fails.
	ssh.fail = func(remote string) error {
		if remote == verifyFirewallCmd {
			return errors.New("exit status 1")
		}
		return nil
	}
	if err := m.Up(context.Background(), true); !errors.Is(err, ErrFirewallMissing) {
		t.Fatalf("Up(reprovision) = %v, want ErrFirewallMissing", err)
	}
	st := mustState(t, m)
	if !slices.Equal(st.Packages, []string{"jq"}) || st.Mode != ModeOnline {
		t.Errorf("state = %+v, want packages [jq] kept and mode online", st)
	}
}

func TestUpCreateClearsStaleLists(t *testing.T) {
	m, _, _ := newTestManager(t)
	stale := &State{SSHPort: 42022, Mode: ModeOffline,
		Images: []string{"x"}, JDKs: []string{"17"}}
	if err := SaveState(m.Layout.StatePath, stale); err != nil {
		t.Fatal(err)
	}
	mustUp(t, m)
	if st := mustState(t, m); len(st.Images)+len(st.JDKs) != 0 {
		t.Errorf("state = %+v, want lists cleared", st)
	}
}

func TestUpCreateDropsStaleKnownHosts(t *testing.T) {
	m, _, _ := newTestManager(t)
	const staleEntry = "[127.0.0.1]:42022 ssh-ed25519 AAAAstale\n"
	if err := os.MkdirAll(filepath.Dir(m.Layout.KnownHosts), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Layout.KnownHosts, []byte(staleEntry), 0o600); err != nil {
		t.Fatal(err)
	}
	mustUp(t, m)
	got, err := os.ReadFile(m.Layout.KnownHosts)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "AAAAstale") {
		t.Errorf("known_hosts = %q, want the stale entry gone", got)
	}
}

func TestUpFailsWhenFirewallMissingOffline(t *testing.T) {
	m, _, ssh := newTestManager(t)
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

// The forwarded ssh port is baked into the instance's lima.yaml, so a new
// port picked for a lost state.json would point every later attach elsewhere.
func TestUpRefusesExistingInstanceWithoutState(t *testing.T) {
	m, lima, ssh := newTestManager(t)
	mustUp(t, m)
	if err := os.Remove(m.Layout.StatePath); err != nil {
		t.Fatal(err)
	}
	m.Deps.PortFree = func(p int) bool { return p != 42022 } // Lima holds it
	lima.reset()
	nSSH := len(ssh.remotes())

	err := m.Up(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "is missing; run `omac vm reset`") {
		t.Fatalf("Up = %v, want the missing-state error", err)
	}
	assertVerbs(t, lima, "list")
	if n := len(ssh.remotes()) - nSSH; n != 0 {
		t.Errorf("ran %d ssh commands, want none", n)
	}
	if _, err := os.Stat(m.Layout.StatePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state.json recreated (err %v)", err)
	}
}

func TestUpMissingLimactl(t *testing.T) {
	m, lima, _ := newTestManager(t)
	m.Deps.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if err := m.Up(context.Background(), false); !errors.Is(err, ErrLimaMissing) {
		t.Fatalf("Up = %v, want ErrLimaMissing", err)
	}
	assertVerbs(t, lima)
}

func TestUpReprovisionRewritesPackagesAndCycles(t *testing.T) {
	m, lima, ssh := newTestManager(t)
	mustUp(t, m)
	writeConfig(t, m, "packages: [jq]\n")
	lima.reset()

	m.Deps.ProvisionTimeout = 2 * time.Second
	if err := m.Up(context.Background(), true); err != nil {
		t.Fatalf("Up(reprovision): %v", err)
	}
	assertVerbs(t, lima, "list", "stop", "start", "stop", "start")
	if got := lima.calls[2].args; !slices.Equal(got, []string{"start", "--tty=false", "--timeout", "2s", InstanceName}) {
		t.Errorf("reprovisioning start args = %v, want the provisioning timeout", got)
	}
	if got := lima.calls[4].args; !slices.Equal(got, []string{"start", "--tty=false", "--timeout", "1s", InstanceName}) {
		t.Errorf("offline start args = %v, want the restart timeout", got)
	}
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

func TestUpInstallsJDKPackages(t *testing.T) {
	m, _, _ := newTestManager(t)
	writeConfig(t, m, "packages: [jq]\njdks: [temurin-25]\n")
	mustUp(t, m)

	yaml, err := os.ReadFile(m.Layout.LimaYAML)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(yaml), "jq temurin-25-jdk") {
		t.Errorf("lima yaml lacks the package line %q:\n%s", "jq temurin-25-jdk", yaml)
	}
	st := mustState(t, m)
	if !slices.Equal(st.JDKs, []string{"temurin-25"}) || !slices.Equal(st.Packages, []string{"jq"}) {
		t.Errorf("state jdks = %v, packages = %v; want [temurin-25], [jq]", st.JDKs, st.Packages)
	}
	r, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r.Write(&out)
	if !strings.Contains(out.String(), "jdks:\n  - temurin-25\n") {
		t.Errorf("status output lacks the jdk:\n%s", out.String())
	}
}

func TestUpCreateRecordsJDKsOnlyAfterProvisioning(t *testing.T) {
	m, _, ssh := newTestManager(t)
	writeConfig(t, m, "jdks: [temurin-25]\n")
	ssh.fail = func(remote string) error {
		if remote == verifyFirewallCmd {
			return errors.New("exit status 1")
		}
		return nil
	}
	if err := m.Up(context.Background(), false); !errors.Is(err, ErrFirewallMissing) {
		t.Fatalf("Up = %v, want ErrFirewallMissing", err)
	}
	if st := mustState(t, m); len(st.JDKs) != 0 {
		t.Errorf("state jdks = %v after a failed provisioning, want none", st.JDKs)
	}
}

func TestUpReprovisionAddsJDK(t *testing.T) {
	m, _, ssh := newTestManager(t)
	writeConfig(t, m, "packages: [jq]\n")
	mustUp(t, m)
	writeConfig(t, m, "packages: [jq]\njdks: [temurin-21]\n")

	if err := m.Up(context.Background(), true); err != nil {
		t.Fatalf("Up(reprovision): %v", err)
	}
	if _, g := ssh.snapshot(); g.packages != "jq temurin-21-jdk" {
		t.Errorf("guest packages = %q, want %q", g.packages, "jq temurin-21-jdk")
	}
	st := mustState(t, m)
	if !slices.Equal(st.JDKs, []string{"temurin-21"}) || !slices.Equal(st.Packages, []string{"jq"}) {
		t.Errorf("state jdks = %v, packages = %v; want [temurin-21], [jq]", st.JDKs, st.Packages)
	}
}

func TestUpRejectsUnsupportedJDKBeforeLima(t *testing.T) {
	m, lima, _ := newTestManager(t)
	writeConfig(t, m, "jdks: [openjdk-21]\n")
	err := m.Up(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), `unsupported jdk "openjdk-21"`) {
		t.Fatalf("Up = %v, want an unsupported jdk error", err)
	}
	assertVerbs(t, lima)
}

func TestUpReprovisionStartsStoppedInstanceFirst(t *testing.T) {
	m, lima, _ := newTestManager(t)
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
	m, lima, _ := newTestManager(t)
	mustUp(t, m)
	writeConfig(t, m, "packages: [\"jq; reboot\"]\n")
	lima.reset()
	if err := m.Up(context.Background(), true); err == nil || !strings.HasPrefix(err.Error(), "omac vm:") {
		t.Fatalf("Up = %v, want an omac vm: error", err)
	}
	assertVerbs(t, lima, "list")
}

func TestDownStopsAndKeepsState(t *testing.T) {
	m, lima, _ := newTestManager(t)
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
	m, _, _ := newTestManager(t)
	if err := m.Down(context.Background()); !errors.Is(err, ErrNotCreated) {
		t.Fatalf("Down = %v, want ErrNotCreated", err)
	}
}

func TestResetDeletesEverythingThenUp(t *testing.T) {
	m, lima, _ := newTestManager(t)
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
	if _, err := os.Stat(filepath.Dir(stray)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s survived reset (err %v)", filepath.Dir(stray), err)
	}
	// Markers are pruned by process ID, never deleted by reset.
	if _, err := os.Stat(l.SessionsDir); err != nil {
		t.Errorf("sessions dir removed by reset: %v", err)
	}
	if lima.status() != StatusRunning {
		t.Errorf("status = %q, want running", lima.status())
	}
}

func TestResetWithoutInstanceSkipsDelete(t *testing.T) {
	m, lima, _ := newTestManager(t)
	if err := m.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if slices.Contains(lima.verbs(), "delete") {
		t.Errorf("verbs = %v, want no delete", lima.verbs())
	}
}

func TestSetModeOnlineRefusesWithSessions(t *testing.T) {
	m, lima, _ := newTestManager(t)
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

// A session that registers after the first marker check (here: during the
// instance lookup) is caught by the second check, before any restart.
func TestSetModeOnlineRefusesSessionRegisteredMidSwitch(t *testing.T) {
	m, lima, _ := newTestManager(t)
	mustUp(t, m)
	m.Deps.Alive = func(pid int) bool { return pid == 5151 }
	lima.reset()
	registered := false
	m.Deps.Lima = func(ctx context.Context, home string, stdout io.Writer, args ...string) error {
		if args[0] == "list" && !registered {
			registered = true
			if _, err := RegisterSession(m.Layout, 5151); err != nil {
				t.Error(err)
			}
		}
		return lima.run(ctx, home, stdout, args...)
	}

	err := m.SetMode(context.Background(), ModeOnline)
	if !errors.Is(err, ErrSessionsAttached) || !strings.Contains(err.Error(), "5151") {
		t.Fatalf("SetMode(online) = %v, want ErrSessionsAttached naming 5151", err)
	}
	assertVerbs(t, lima, "list")
	if st := mustState(t, m); st.Mode != ModeOffline {
		t.Errorf("mode = %q, want offline restored", st.Mode)
	}
}

// attachLiveSession registers pid as a live session and clears the call logs.
func attachLiveSession(t *testing.T, m *Manager, lima *fakeLimaCtl, pid int) {
	t.Helper()
	if _, err := RegisterSession(m.Layout, pid); err != nil {
		t.Fatal(err)
	}
	m.Deps.Alive = func(p int) bool { return p == pid }
	lima.reset()
}

// Every path that takes the VM online refuses while a session is attached,
// before any ssh command or limactl verb beyond the instance lookup.
func TestOnlineTransitionsRefuseAttachedSessions(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, m *Manager, lima *fakeLimaCtl)
		run   func(m *Manager) error
		verbs []string // limactl verbs allowed before the refusal
	}{
		{
			name:  "up --reprovision",
			setup: func(t *testing.T, m *Manager, _ *fakeLimaCtl) { mustUp(t, m) },
			run:   func(m *Manager) error { return m.Up(context.Background(), true) },
			verbs: []string{"list"},
		},
		{
			name: "up re-running interrupted provisioning",
			setup: func(t *testing.T, m *Manager, lima *fakeLimaCtl) {
				lima.failOn["start"] = errors.New("boot timed out")
				if err := m.Up(context.Background(), false); err == nil {
					t.Fatal("setup: Up succeeded, want the interrupted create")
				}
				delete(lima.failOn, "start")
			},
			run:   func(m *Manager) error { return m.Up(context.Background(), false) },
			verbs: []string{"list"},
		},
		{
			name:  "reset",
			setup: func(t *testing.T, m *Manager, _ *fakeLimaCtl) { mustUp(t, m) },
			run:   func(m *Manager) error { return m.Reset(context.Background()) },
			verbs: nil, // refuses before the instance lookup
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, lima, ssh := newTestManager(t)
			c.setup(t, m, lima)
			adminKey, _ := os.ReadFile(m.Layout.AdminKey)
			attachLiveSession(t, m, lima, 4343)
			nSSH := len(ssh.remotes())

			err := c.run(m)
			if !errors.Is(err, ErrSessionsAttached) || !strings.Contains(err.Error(), "4343") {
				t.Fatalf("err = %v, want ErrSessionsAttached naming 4343", err)
			}
			assertVerbs(t, lima, c.verbs...)
			if got := ssh.remotes()[nSSH:]; len(got) != 0 {
				t.Errorf("ssh commands after refusal: %v", got)
			}
			if pids, _ := AttachedSessions(m.Layout, m.Deps.Alive); !slices.Equal(pids, []int{4343}) {
				t.Errorf("attached sessions = %v, want the marker kept", pids)
			}
			if key, _ := os.ReadFile(m.Layout.AdminKey); !bytes.Equal(key, adminKey) {
				t.Error("admin key changed by a refused command")
			}
		})
	}
}

func TestSetModeOnlineCyclesAndVerifiesNoFirewall(t *testing.T) {
	m, lima, ssh := newTestManager(t)
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
	m, _, ssh := newTestManager(t)
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
	m, _, ssh := newTestManager(t)
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
	m, lima, _ := newTestManager(t)
	if err := m.SetMode(context.Background(), "airplane"); err == nil {
		t.Fatal("SetMode(airplane) succeeded")
	}
	assertVerbs(t, lima)
}

func TestStatusReportsEverything(t *testing.T) {
	m, _, _ := newTestManager(t)
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
		Created: true, Running: true, Mode: ModeOffline, SSHPort: 42022,
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
	m, _, ssh := newTestManager(t)
	mustUp(t, m)
	ssh.fail = dockerDown
	r, err := m.Status(context.Background())
	if err != nil || r.Docker != nil || !r.Running {
		t.Fatalf("Status = %+v, %v; want running with Docker nil", r, err)
	}
}

func TestStatusListsWorktreesFromGuest(t *testing.T) {
	m, lima, ssh := newTestManager(t)
	ctx := context.Background()
	mustUp(t, m)
	// Two concurrent first runs can both append the same line.
	ssh.setWorktrees("a1b2c3 /Users/alice/proj\n\nd4e5f6 /Users/alice/my proj\na1b2c3 /Users/alice/proj\n")

	r, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	want := []string{"/Users/alice/proj (a1b2c3)", "/Users/alice/my proj (d4e5f6)"}
	if !slices.Equal(r.Worktrees, want) {
		t.Errorf("Worktrees = %q, want %q", r.Worktrees, want)
	}
	calls, _ := ssh.snapshot()
	var listed bool
	for _, c := range calls {
		if c.remote == listWorktreesCmd {
			listed = true
			if c.target.User != AgentUserName("alice") {
				t.Errorf("listed worktrees as %q, want the agent user", c.target.User)
			}
		}
	}
	if !listed {
		t.Errorf("Status never ran %q; remotes = %q", listWorktreesCmd, ssh.remotes())
	}

	// An SSH failure makes the list unknown, not empty.
	ssh.fail = func(remote string) error {
		if remote == listWorktreesCmd {
			return errors.New("exit status 255")
		}
		return nil
	}
	if r, err = m.Status(ctx); err != nil || len(r.Worktrees) != 0 || !strings.HasPrefix(r.WorktreesNote, "unknown") {
		t.Errorf("Status with failing ssh = %q, note %q, %v; want unknown, no error", r.Worktrees, r.WorktreesNote, err)
	}
	ssh.fail = nil

	// A stopped VM is not asked.
	lima.setStatus(StatusStopped)
	before := len(ssh.remotes())
	if r, err = m.Status(ctx); err != nil || len(r.Worktrees) != 0 || r.WorktreesNote != "unknown (VM not running)" {
		t.Errorf("Status of stopped VM = %q, note %q, %v; want unknown (VM not running), no error", r.Worktrees, r.WorktreesNote, err)
	}
	if after := ssh.remotes(); len(after) != before {
		t.Errorf("Status of stopped VM ran ssh: %q", after[before:])
	}
}

func TestImageAddPullsLoadsAndRecords(t *testing.T) {
	m, _, ssh := newTestManager(t)
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
	m, lima, _ := newTestManager(t)
	err := m.ImageAdd(context.Background(), "not a ref!")
	if err == nil || !strings.HasPrefix(err.Error(), "omac vm:") {
		t.Fatalf("ImageAdd = %v, want omac vm: error", err)
	}
	assertVerbs(t, lima)
}

func TestImageListMergesStateAndVM(t *testing.T) {
	m, _, ssh := newTestManager(t)
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
	m, _, ssh := newTestManager(t)
	ctx := context.Background()
	mustUp(t, m)
	if err := m.ImageAdd(ctx, "alpine"); err != nil {
		t.Fatal(err)
	}
	if err := m.ImageRemove(ctx, "alpine"); err != nil {
		t.Fatalf("ImageRemove: %v", err)
	}
	if !slices.Contains(ssh.remotes(), imageRemoveCmd("index.docker.io/library/alpine:latest")) {
		t.Errorf("no docker image rm in %v", ssh.remotes())
	}
	if _, g := ssh.snapshot(); len(g.images) != 0 {
		t.Errorf("guest images = %v, want none", g.images)
	}
	if st := mustState(t, m); len(st.Images) != 0 {
		t.Errorf("state images = %v, want none", st.Images)
	}
}

// An image deleted inside the VM (docker image rm, docker system prune)
// still drops out of state.
func TestImageRemoveMissingImageClearsState(t *testing.T) {
	m, _, ssh := newTestManager(t)
	ctx := context.Background()
	mustUp(t, m)
	if err := m.ImageAdd(ctx, "alpine"); err != nil {
		t.Fatal(err)
	}
	ssh.mu.Lock()
	ssh.guest.images = nil
	ssh.mu.Unlock()

	if err := m.ImageRemove(ctx, "alpine"); err != nil {
		t.Fatalf("ImageRemove of a missing image: %v", err)
	}
	if st := mustState(t, m); len(st.Images) != 0 {
		t.Errorf("state images = %v, want none", st.Images)
	}
}

// Each operation fails part-way through; state and host files must stay
// what they were, so a retry starts from a truthful state.
func TestManagerFailureBranches(t *testing.T) {
	boom := errors.New("boom")
	imagesEmpty := func(t *testing.T, m *Manager) {
		t.Helper()
		if entries, _ := os.ReadDir(filepath.Join(m.Layout.Root, "images")); len(entries) != 0 {
			t.Errorf("tarball left behind: %v", entries)
		}
		if st := mustState(t, m); len(st.Images) != 0 {
			t.Errorf("state images = %v, want none", st.Images)
		}
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, m *Manager, lima *fakeLimaCtl, ssh *fakeSSH)
		run   func(m *Manager) error
		check func(t *testing.T, m *Manager, lima *fakeLimaCtl, ssh *fakeSSH)
	}{
		{
			name: "image add, pull fails",
			setup: func(t *testing.T, m *Manager, _ *fakeLimaCtl, _ *fakeSSH) {
				m.Deps.Pull = func(_ context.Context, _, tar string) error {
					if err := os.WriteFile(tar, []byte("partial"), 0o600); err != nil {
						t.Fatal(err)
					}
					return boom
				}
			},
			run: func(m *Manager) error { return m.ImageAdd(context.Background(), "alpine") },
			check: func(t *testing.T, m *Manager, _ *fakeLimaCtl, ssh *fakeSSH) {
				imagesEmpty(t, m)
				if slices.Contains(ssh.remotes(), dockerCmd("load")) {
					t.Error("docker load ran after a failed pull")
				}
			},
		},
		{
			name: "image add, docker load fails",
			setup: func(_ *testing.T, _ *Manager, _ *fakeLimaCtl, ssh *fakeSSH) {
				ssh.fail = func(remote string) error {
					if remote == dockerCmd("load") {
						return boom
					}
					return nil
				}
			},
			run: func(m *Manager) error { return m.ImageAdd(context.Background(), "alpine") },
			check: func(t *testing.T, m *Manager, _ *fakeLimaCtl, ssh *fakeSSH) {
				imagesEmpty(t, m)
				if _, g := ssh.snapshot(); len(g.images) != 0 {
					t.Errorf("guest images = %v, want none", g.images)
				}
			},
		},
		{
			name: "image rm on a stopped VM",
			setup: func(t *testing.T, m *Manager, _ *fakeLimaCtl, _ *fakeSSH) {
				if err := m.ImageAdd(context.Background(), "alpine"); err != nil {
					t.Fatal(err)
				}
				if err := m.Down(context.Background()); err != nil {
					t.Fatal(err)
				}
			},
			run: func(m *Manager) error { return m.ImageRemove(context.Background(), "alpine") },
			check: func(t *testing.T, m *Manager, _ *fakeLimaCtl, _ *fakeSSH) {
				if st := mustState(t, m); !slices.Equal(st.Images, []string{"index.docker.io/library/alpine:latest"}) {
					t.Errorf("state images = %v, want alpine kept", st.Images)
				}
			},
		},
		{
			name:  "down, limactl stop fails",
			setup: func(_ *testing.T, _ *Manager, lima *fakeLimaCtl, _ *fakeSSH) { lima.failOn["stop"] = boom },
			run:   func(m *Manager) error { return m.Down(context.Background()) },
			check: func(t *testing.T, m *Manager, lima *fakeLimaCtl, _ *fakeSSH) {
				if lima.status() != StatusRunning {
					t.Errorf("status = %q, want running", lima.status())
				}
				if st := mustState(t, m); st.Mode != ModeOffline {
					t.Errorf("mode = %q, want offline kept", st.Mode)
				}
			},
		},
		{
			name: "offline switch, stop fails after the mode file was written",
			setup: func(t *testing.T, m *Manager, lima *fakeLimaCtl, _ *fakeSSH) {
				if err := m.SetMode(context.Background(), ModeOnline); err != nil {
					t.Fatal(err)
				}
				lima.failOn["stop"] = boom
			},
			run: func(m *Manager) error { return m.SetMode(context.Background(), ModeOffline) },
			check: func(t *testing.T, m *Manager, _ *fakeLimaCtl, ssh *fakeSSH) {
				if _, g := ssh.snapshot(); g.modeFile != ModeOffline {
					t.Fatalf("setup: guest mode file = %q, want offline written", g.modeFile)
				}
				if st := mustState(t, m); st.Mode != ModeOnline {
					t.Errorf("mode = %q, want online until the firewall is verified", st.Mode)
				}
			},
		},
		{
			name:  "reset, limactl delete fails",
			setup: func(_ *testing.T, _ *Manager, lima *fakeLimaCtl, _ *fakeSSH) { lima.failOn["delete"] = boom },
			run:   func(m *Manager) error { return m.Reset(context.Background()) },
			check: func(t *testing.T, m *Manager, lima *fakeLimaCtl, _ *fakeSSH) {
				l := m.Layout
				for _, p := range []string{l.StatePath, l.AdminKey, l.AdminKey + ".pub", l.AgentKey, l.AgentKey + ".pub", l.LimaYAML} {
					if _, err := os.Stat(p); err != nil {
						t.Errorf("%s removed although the VM still exists: %v", p, err)
					}
				}
				if lima.status() == "" {
					t.Error("instance gone")
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, lima, ssh := newTestManager(t)
			mustUp(t, m)
			c.setup(t, m, lima, ssh)
			if err := c.run(m); err == nil {
				t.Fatal("err = nil, want a failure")
			}
			c.check(t, m, lima, ssh)
		})
	}
}

func TestPrepareAttachChecks(t *testing.T) {
	m, _, ssh := newTestManager(t)
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
	ssh.fail = dockerDown
	if _, err := m.PrepareAttach(ctx); err == nil || !strings.Contains(err.Error(), "docker in the VM") {
		t.Fatalf("docker down: %v", err)
	}
	ssh.fail = nil

	info, err := m.PrepareAttach(ctx)
	if err != nil {
		t.Fatalf("PrepareAttach: %v", err)
	}
	want := AttachInfo{SSHPort: 42022, AgentKeyPath: m.Layout.AgentKey, User: "alice",
		Docker: DockerVersion{APIVersion: "1.47", Version: "27.3.1"}}
	if info != want {
		t.Errorf("AttachInfo = %+v, want %+v", info, want)
	}
}

func TestPrepareAttachRequiresLoadedFirewall(t *testing.T) {
	m, _, ssh := newTestManager(t)
	ctx := context.Background()
	mustUp(t, m)
	ssh.setFirewall(false) // state says offline, but the table is gone
	nSSH := len(ssh.remotes())

	_, err := m.PrepareAttach(ctx)
	if !errors.Is(err, ErrFirewallMissing) {
		t.Fatalf("PrepareAttach = %v, want ErrFirewallMissing", err)
	}
	if slices.Contains(ssh.remotes()[nSSH:], dockerVersionCmd) {
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
		Created: true, Running: true, Mode: ModeOffline, SSHPort: 42022,
		Resources: Resources{CPUs: 8, Memory: "16GiB", Disk: "60GiB"},
		Images:    []string{"index.docker.io/library/alpine:latest", "index.docker.io/library/busybox:1.36"},
		// The agent user writes the registry and answers the Docker probe.
		Worktrees: []string{"/Users/alice/proj (a1b2c3)", "/x\x1b]52;c;Zm9v\x07\nmode: online (s)"},
		JDKs:      []string{"temurin-25"},
		Docker:    &DockerVersion{APIVersion: "1.47", Version: "27.3.1\x1b[2J"},
	}
	var b bytes.Buffer
	r.Write(&b)
	out := b.String()
	for _, want := range []string{
		"status: running", "mode: offline", "ssh: 127.0.0.1:42022", "docker: Docker 27.3.1\\x1b[2J, API 1.47\n",
		"images:\n  - index.docker.io/library/alpine:latest\n  - index.docker.io/library/busybox:1.36\n",
		"worktrees:\n  - /Users/alice/proj (a1b2c3)\n  - /x\\x1b]52;c;Zm9v\\x07\\nmode: online (s)\n",
		"jdks:\n  - temurin-25\n",
		"sessions: none",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("report passes control characters through: %q", out)
	}

	b.Reset()
	StatusReport{Created: true, WorktreesNote: "unknown (VM not running)"}.Write(&b)
	for _, want := range []string{"status: stopped", "docker: not answering\n", "worktrees: unknown (VM not running)", "jdks: none"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("stopped report lacks %q:\n%s", want, b.String())
		}
	}

	b.Reset()
	StatusReport{}.Write(&b)
	if !strings.Contains(b.String(), "status: not created") {
		t.Errorf("empty report = %q", b.String())
	}
}
