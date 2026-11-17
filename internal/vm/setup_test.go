package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// hostile is a setup script that would break a naive quoting or heredoc
// embedding.
const hostile = "echo 'it'\"s\" $HOME `id`\ncat <<'EOF'\nEOF\nOMACVMGUARD\n$(reboot)\n"

func yamlSetup(script string) (string, error) {
	b, err := yaml.Marshal(map[string]string{"setup": script})
	return string(b), err
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func setupCalls(ssh *fakeSSH) []sshCall {
	calls, _ := ssh.snapshot()
	var out []sshCall
	for _, c := range calls {
		if c.remote == setupCmd {
			out = append(out, c)
		}
	}
	return out
}

func TestLoadUserConfigSetup(t *testing.T) {
	m, _, _ := newTestManager(t)
	writeConfig(t, m, "setup: |\n  echo hi\n")
	cfg, err := LoadUserConfig(m.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Setup != "echo hi\n" {
		t.Errorf("Setup = %q", cfg.Setup)
	}
}

func TestUpCreateRunsSetupOnceAsRootBetweenProvisioningAndOffline(t *testing.T) {
	m, _, ssh := newTestManager(t)
	cfgYAML, err := yamlSetup(hostile)
	if err != nil {
		t.Fatal(err)
	}
	writeConfig(t, m, cfgYAML)
	mustUp(t, m)

	calls := setupCalls(ssh)
	if len(calls) != 1 {
		t.Fatalf("setup ran %d times, want 1", len(calls))
	}
	if calls[0].stdin != hostile {
		t.Errorf("setup stdin = %q, want the script verbatim", calls[0].stdin)
	}
	if calls[0].target.User != "root" {
		t.Errorf("setup ran as %q, want root", calls[0].target.User)
	}
	remotes := ssh.remotes()
	setupAt := slices.Index(remotes, setupCmd)
	offlineAt := slices.Index(remotes, writeModeCmd(ModeOffline))
	dockerAt := slices.Index(remotes, dockerVersionCmd)
	if !(dockerAt >= 0 && dockerAt < setupAt && setupAt < offlineAt) {
		t.Errorf("order wrong: docker %d, setup %d, offline %d in %v", dockerAt, setupAt, offlineAt, remotes)
	}
}

func TestUpCreateWithoutSetupMakesNoSetupCall(t *testing.T) {
	m, _, ssh := newTestManager(t)
	mustUp(t, m)
	if n := len(setupCalls(ssh)); n != 0 {
		t.Errorf("setup ran %d times without a setup field", n)
	}
}

func TestSetupNeverReachesLimaConfig(t *testing.T) {
	m, _, _ := newTestManager(t)
	cfgYAML, err := yamlSetup(hostile)
	if err != nil {
		t.Fatal(err)
	}
	writeConfig(t, m, cfgYAML)
	mustUp(t, m)
	yaml := readFile(t, m.Layout.LimaYAML)
	if strings.Contains(yaml, "reboot") {
		t.Error("lima config contains the setup script")
	}
}

func TestUpReprovisionRunsSetupAgainBeforeOffline(t *testing.T) {
	m, _, ssh := newTestManager(t)
	mustUp(t, m)
	writeConfig(t, m, "setup: |\n  echo new\n")
	if err := m.Up(context.Background(), true); err != nil {
		t.Fatalf("Up(reprovision): %v", err)
	}
	calls := setupCalls(ssh)
	if len(calls) != 1 || calls[0].stdin != "echo new\n" {
		t.Fatalf("setup calls = %+v, want one with the new script", calls)
	}
	remotes := ssh.remotes()
	setupAt := slices.Index(remotes, setupCmd)
	lastOffline := slices.Index(remotes[setupAt:], writeModeCmd(ModeOffline))
	if lastOffline < 0 {
		t.Errorf("no offline switch after setup in %v", remotes)
	}
	if _, g := ssh.snapshot(); !g.provisioned || !g.firewall {
		t.Errorf("guest = %+v, want provisioned and offline", g)
	}
}

func TestUpReprovisionWithoutSetupMakesNoSetupCall(t *testing.T) {
	m, _, ssh := newTestManager(t)
	mustUp(t, m)
	if err := m.Up(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if n := len(setupCalls(ssh)); n != 0 {
		t.Errorf("setup ran %d times", n)
	}
}

func TestSetupFailureRemovesMarkerAndShowsOutput(t *testing.T) {
	m, _, ssh := newTestManager(t)
	writeConfig(t, m, "setup: |\n  false\n")
	ssh.fail = func(remote string) error {
		if remote == setupCmd {
			return errors.New("exit status 7")
		}
		return nil
	}
	err := m.Up(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "setup") || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("Up = %v, want a setup failure", err)
	}
	if !strings.Contains(err.Error(), "omac vm up") {
		t.Errorf("error %q lacks the retry hint", err)
	}
	_, g := ssh.snapshot()
	if g.provisioned {
		t.Error("marker still present after a failed setup")
	}
	if st := mustState(t, m); st.Mode != ModeOnline {
		t.Errorf("state mode = %q, want online", st.Mode)
	}
	if slices.Contains(ssh.remotes(), writeModeCmd(ModeOffline)) {
		t.Error("VM went offline despite the failed setup")
	}
}

func TestSetupFailureOutputIsCapped(t *testing.T) {
	big := strings.Repeat("x", 100000)
	got := capOutput(big)
	if len(got) > 5000 {
		t.Errorf("capped output is %d bytes", len(got))
	}
}

func TestCapOutputKeepsValidUTF8(t *testing.T) {
	got := capOutput(strings.Repeat("ä", maxSetupOutput))
	if !utf8.ValidString(got) {
		t.Error("capped output is not valid UTF-8")
	}
}

func TestSetupFailureWithHugeStderrYieldsCappedError(t *testing.T) {
	m, _, ssh := newTestManager(t)
	writeConfig(t, m, "setup: |\n  false\n")
	ssh.fail = func(remote string) error {
		if remote == setupCmd {
			return errors.New("exit status 1: " + strings.Repeat("apt error ", 20000))
		}
		return nil
	}
	err := m.Up(context.Background(), false)
	if err == nil {
		t.Fatal("Up succeeded")
	}
	if len(err.Error()) > 6000 {
		t.Errorf("error is %d bytes, want it capped", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "apt error") || !strings.Contains(err.Error(), "omac vm up") {
		t.Errorf("error lost the tail or the hint: %.200s", err)
	}
}

// Ctrl-C during the script cancels ctx, so no cleanup command could run
// afterwards; the marker must already be gone while the script runs.
func TestInterruptedSetupLeavesNoMarker(t *testing.T) {
	m, _, ssh := newTestManager(t)
	writeConfig(t, m, "setup: |\n  sleep 600\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ssh.fail = func(remote string) error {
		if remote == setupCmd {
			cancel()
			return errors.New("signal: killed")
		}
		return nil
	}
	if err := m.Up(ctx, false); err == nil {
		t.Fatal("Up succeeded")
	}
	if _, g := ssh.snapshot(); g.provisioned {
		t.Error("marker present after an interrupted setup; the next up would skip setup")
	}
}

// The script's own output says why it failed; both streams have to reach
// the error, and of a long output its end.
func TestSetupFailureShowsTheEndOfBothStreams(t *testing.T) {
	m, _, _ := newTestManager(t)
	writeConfig(t, m, "setup: |\n  false\n")
	fake := m.Deps.SSH
	m.Deps.SSH = func(ctx context.Context, tg SSHTarget, stdin io.Reader, stdout, stderr io.Writer, remote string) error {
		if remote != setupCmd {
			return fake(ctx, tg, stdin, stdout, stderr, remote)
		}
		fmt.Fprint(stdout, strings.Repeat("early line\n", 2000))
		fmt.Fprint(stdout, "last stdout line\n")
		fmt.Fprint(stderr, "E: Unable to locate package nope\n")
		return errors.New("exit status 100")
	}
	err := m.Up(context.Background(), false)
	if err == nil {
		t.Fatal("Up succeeded")
	}
	for _, want := range []string{"last stdout line", "Unable to locate package nope", "exit status 100", "…"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %.300s", want, err)
		}
	}
	if len(err.Error()) > 2*maxSetupOutput {
		t.Errorf("error is %d bytes, want it capped", len(err.Error()))
	}
}

func TestSetupOutputKeepsOnlyTheEnd(t *testing.T) {
	var w tailWriter
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&w, "line %d %s\n", i, strings.Repeat("x", 100))
	}
	if len(w.b) > 2*maxSetupOutput {
		t.Errorf("kept %d bytes, want at most %d", len(w.b), 2*maxSetupOutput)
	}
	if got := w.String(); !strings.HasPrefix(got, "…") || !strings.Contains(got, "line 999 ") {
		t.Errorf("output lost the end or the cut mark: %.80q", got)
	}
}

func TestReprovisionSetupFailureAfterOnlineSwitchRemovesMarker(t *testing.T) {
	m, _, ssh := newTestManager(t)
	mustUp(t, m)
	writeConfig(t, m, "setup: |\n  false\n")
	ssh.fail = func(remote string) error {
		if remote == setupCmd {
			return errors.New("exit status 3")
		}
		return nil
	}
	err := m.Up(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("Up(reprovision) = %v, want the setup failure", err)
	}
	remotes := ssh.remotes()
	setupAt := slices.Index(remotes, setupCmd)
	onlineAt := slices.Index(remotes, writeModeCmd(ModeOnline))
	if onlineAt < 0 || setupAt < onlineAt {
		t.Errorf("setup (%d) must follow the online switch (%d): %v", setupAt, onlineAt, remotes)
	}
	if offlineAfter := slices.Index(remotes[setupAt:], writeModeCmd(ModeOffline)); offlineAfter >= 0 {
		t.Error("VM went offline after the failed setup")
	}
	if _, g := ssh.snapshot(); g.provisioned {
		t.Error("marker present after a failed setup")
	}
	if st := mustState(t, m); st.Mode != ModeOnline {
		t.Errorf("state mode = %q, want online", st.Mode)
	}
}
