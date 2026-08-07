package buildrun

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStopGradleDaemon_CooperativeStopUsesIsolatedEnv asserts S6: the
// `gradlew --stop` child runs with an isolated env (no host HOME, leaf
// as GRADLE_USER_HOME) — NOT os.Environ(). Uses a stub wrapper that
// records its GRADLE_USER_HOME and the absence of HOME.
func TestStopGradleDaemon_CooperativeStopUsesIsolatedEnv(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	leaf := t.TempDir()
	wt := t.TempDir()
	// A host secret in the env must not reach the stop child.
	t.Setenv("SHOULD_NOT_LEAK", "host-secret")

	marker := filepath.Join(wt, "stop-env")
	wrapper := "#!/bin/sh\n" +
		"echo \"GUH=$GRADLE_USER_HOME\" >> " + marker + "\n" +
		"echo \"HOME=${HOME:-unset}\" >> " + marker + "\n" +
		"echo \"LEAK=${SHOULD_NOT_LEAK:-unset}\" >> " + marker + "\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(wt, "gradlew"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}

	err := StopGradleDaemon(StopDaemonOptions{
		Wrapper:    filepath.Join(wt, "gradlew"),
		ProjectDir: wt,
		Leaf:       leaf,
		// No Grants: minimal leaf-only env (still no HOME).
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("StopGradleDaemon: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("wrapper not invoked (marker missing): %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "GUH="+leaf) {
		t.Errorf("GRADLE_USER_HOME = %q, want leaf %q (S6: isolated env)", s, leaf)
	}
	// HOME must be absent (the spec-critical boundary: no host ~/.gradle).
	if !strings.Contains(s, "HOME=unset") {
		t.Errorf("stop child inherited HOME (S6 violation): %q", s)
	}
	// A host env secret must not leak.
	if strings.Contains(s, "LEAK=host-secret") {
		t.Errorf("stop child leaked host env secret (S6): %q", s)
	}
}

// TestStopGradleDaemon_ForceKillsLingeringDaemon asserts S7 (spec.md:146):
// after the cooperative `gradlew --stop`, a lingering daemon process for
// the leaf is SIGKILLed. We plant a fake daemon registry pointing at a
// stub java process that ignores SIGTERM, and verify StopGradleDaemon
// force-kills it. (Uses a stub process, not a real Gradle daemon.)
func TestStopGradleDaemon_ForceKillsLingeringDaemon(t *testing.T) {
	leaf := t.TempDir()
	wt := t.TempDir()

	// Stub "daemon": a long-running script INSIDE the leaf dir so the
	// leaf path appears in its `ps`/cmdline (processAliveAndAssociated
	// matches on the leaf being in the command line). It ignores SIGTERM
	// so only the S7 force-kill (SIGKILL) ends it.
	stubScript := filepath.Join(leaf, "fake-daemon.sh")
	if err := os.WriteFile(stubScript, []byte("#!/bin/sh\ntrap '' TERM\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	daemon := exec.Command("/bin/sh", stubScript)
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	pid := daemon.Process.Pid

	// Plant a fake daemon registry so activeDaemonPIDs finds the pid.
	regVer := filepath.Join(leaf, ".gradle", "daemon", "8.5")
	if err := os.MkdirAll(regVer, 0o755); err != nil {
		t.Fatal(err)
	}
	// registry.bin embeds the pid as a readable integer (our heuristic).
	registry := []byte("daemon pid=" + itoa(pid) + " status=busy")
	if err := os.WriteFile(filepath.Join(regVer, "registry.bin"), registry, 0o644); err != nil {
		t.Fatal(err)
	}

	// Cooperative stop: a stub wrapper that exits 0 instantly (does NOT
	// kill the daemon). The force-kill fallback must then SIGKILL the
	// lingering stub.
	wrapper := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(wt, "gradlew"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}

	err := StopGradleDaemon(StopDaemonOptions{
		Wrapper:        filepath.Join(wt, "gradlew"),
		ProjectDir:     wt,
		Leaf:           leaf,
		Stdout:         io.Discard,
		Stderr:         io.Discard,
		ForceKillAfter: 300 * time.Millisecond,
		// The in-sandbox environment blocks `ps`, so the production
		// cmdline probe returns "" and an unidentifiable pid is
		// conservatively NOT killed. Inject a fake probe that reports
		// the leaf (simulating the host ps path) so the force-kill fires
		// deterministically here; on the host the real probe runs.
		Cmdline: func(pid int) string { return "/bin/sh " + stubScript },
	})
	if err != nil {
		t.Fatalf("StopGradleDaemon: %v", err)
	}

	// The stub daemon must have been SIGKILLed (process gone).
	// Wait should report it was killed by a signal.
	werr := daemon.Wait()
	if werr == nil {
		t.Error("stub daemon was not force-killed (still running): S7 force-kill fallback did not fire")
	}
}

// itoa is a tiny strconv.Itoa without the import (kept local to the test).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
