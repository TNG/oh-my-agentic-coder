//go:build unix

package mcpproc

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestKillEndsTheWholeProcessGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & wait")
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	time.Sleep(100 * time.Millisecond)
	Kill(cmd)
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not end")
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(-pgid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("a process of the group survived")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
