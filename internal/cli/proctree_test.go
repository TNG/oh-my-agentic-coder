package cli

import (
	"bufio"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// processAlive treats a zombie as dead: a container without a reaping init keeps it in the table.
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	stat := string(raw)
	i := strings.LastIndexByte(stat, ')')
	return i < 0 || !strings.HasPrefix(strings.TrimSpace(stat[i+1:]), "Z")
}

func TestParseProcStatPPID(t *testing.T) {
	cases := []struct {
		stat string
		want int
		ok   bool
	}{
		{"42 (opencode) S 7 42 42 0 -1", 7, true},
		{"42 (a b) c) S 9 42", 9, true},
		{"42 (x)", 0, false},
		{"garbage", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseProcStatPPID(tc.stat)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseProcStatPPID(%q) = (%d, %v), want (%d, %v)", tc.stat, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDescendants(t *testing.T) {
	parents := parsePsParents("  1 0\n 10 1\n 11 10\n 12 11\n 13 1\n 14 12\nbad line here\n")
	got := descendants(10, parents)
	slices.Sort(got)
	if want := []int{11, 12, 14}; !slices.Equal(got, want) {
		t.Errorf("descendants(10) = %v, want %v", got, want)
	}
	if got := descendants(13, parents); len(got) != 0 {
		t.Errorf("leaf has no descendants, got %v", got)
	}
}

// A refused launch must not leave a detached child behind: OpenCode's service leaves the group of the process
// omac starts, so stopping only that group is not enough.
func TestStopSandboxTreeStopsDetachedChildren(t *testing.T) {
	if processParents() == nil {
		t.Skip("no process table (neither /proc nor ps)")
	}
	// Detach the background sleep into its own session/process group, like OpenCode's service:
	// setsid where available (Linux), else job control (macOS sh).
	cmd := exec.Command("sh", "-c", "if command -v setsid >/dev/null 2>&1; then setsid sleep 60 & else set -m; sleep 60 & fi; echo $!; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("child pid %q: %v", line, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	// The child detaches itself after the shell printed its pid; wait for that.
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if pgid, _ := syscall.Getpgid(child); pgid != cmd.Process.Pid {
			break
		}
		if time.Now().After(deadline) {
			t.Skip("shell did not detach the background job into its own group")
		}
	}

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	stopSandboxTree(exited, cmd.Process.Pid)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("root did not exit")
	}
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(child) {
		if time.Now().After(deadline) {
			t.Fatalf("detached child %d survived stopSandboxTree", child)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
