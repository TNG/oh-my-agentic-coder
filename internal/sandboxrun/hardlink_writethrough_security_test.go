//go:build linux

package sandboxrun

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// hardlinkProbeEnv marks a re-exec'd test-binary invocation that runs the
// in-sandbox probe for TestSecurityLandlockFsRulesetBlocksHardlinkWriteThrough.
// The probe runs under the production stage2 enforcement stack (Landlock +
// seccomp applied by `omac sandbox stage2`) and reports via exit code:
// 0 means the read-only grant held, non-zero means it was modified
// through a new name.
const hardlinkProbeEnv = "OMAC_HARDLINK_PROBE"

// TestSecurityLandlockFsRulesetBlocksHardlinkWriteThrough asserts that the
// stage2 filesystem ruleset prevents a confined process from giving a
// read-only-granted file a new name in a writable directory and writing
// through that name back into the original. Read-only enforcement must hold
// at the inode level, not only at the mount-flag level: a new name in a
// writable directory must not widen a read-only grant to read-write.
//
// The probe runs under the production stage2 enforcement stack without a
// bubblewrap mount namespace. bwrap's user namespace causes link(2) to
// return EXDEV across separate bind mounts regardless of the Landlock
// ruleset, which would mask the vulnerability and make the test vacuously
// green. Running stage2 directly exercises the Landlock FS ruleset — the
// layer the fix adds — without that interference.
func TestSecurityLandlockFsRulesetBlocksHardlinkWriteThrough(t *testing.T) {
	if mode := os.Getenv(hardlinkProbeEnv); mode == "fs" {
		os.Exit(runHardlinkWriteThroughProbe())
	}
	if abi := LandlockABI(); abi < 4 {
		t.Fatalf("Landlock ABI %d < 4: the filesystem ruleset requires ABI 4 (TRUNCATE/REFER); cannot verify read-only grant enforcement on this kernel — run in the e2e container", abi)
	}

	// A read-only grant tree holding a file the confined process must not
	// be able to mutate, plus a writable workdir it can write inside.
	roRoot := t.TempDir()
	secret := filepath.Join(roRoot, "data")
	const original = "unchanged"
	if err := os.WriteFile(secret, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()

	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Read: []string{roRoot}},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	omac := buildOmac(t)
	testBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Grant the binary directories read access so the Landlock FS ruleset
	// (after the fix) allows executing stage2 and the re-exec'd probe.
	// Before the fix these grants are inert (stage2 installs no FS ruleset).
	g.ReadPaths = append(g.ReadPaths, filepath.Dir(omac), filepath.Dir(testBin))

	linkPath := filepath.Join(wd, "alias")
	canary := filepath.Join(wd, "canary")
	stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
	tail := append(append([]string{}, stage2...), "--", testBin,
		"-test.run=^TestSecurityLandlockFsRulesetBlocksHardlinkWriteThrough$", "-test.v")
	cmd := exec.Command(tail[0], tail[1:]...)
	cmd.Env = append(os.Environ(),
		hardlinkProbeEnv+"=fs",
		"OMAC_PROBE_RO="+secret,
		"OMAC_PROBE_LINK="+linkPath,
		"OMAC_PROBE_CANARY="+canary,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("launch hardlink probe: %v\n%s", err, out)
	}
	// The canary proves the probe actually ran under stage2 enforcement.
	// Without it, a vacuous green (probe never executed because the
	// ruleset blocked the test binary) would go undetected.
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("the probe did not run (canary %q missing, stage2 exit %d): the FS ruleset may have blocked the test binary from executing\n%s", canary, code, out)
	}
	if code != 0 {
		t.Fatalf("the confined process wrote through a read-only grant via a new name in the workdir (probe exit %d):\n%s", code, out)
	}
	// Belt-and-suspenders: even if the probe reported a denial, confirm the
	// host-side original is byte-for-byte unchanged.
	if data, rerr := os.ReadFile(secret); rerr != nil || string(data) != original {
		t.Errorf("the read-only grant file was modified through a workdir alias: content=%q err=%v", data, rerr)
	}
}

// runHardlinkWriteThroughProbe runs inside the enforced stage2 sandbox. It
// tries to give the read-only file a new name in the writable workdir and
// open that name for writing. Exit 0 means every step was denied; exit 1
// means the write reached the original file.
func runHardlinkWriteThroughProbe() int {
	roPath := os.Getenv("OMAC_PROBE_RO")
	linkPath := os.Getenv("OMAC_PROBE_LINK")
	canaryPath := os.Getenv("OMAC_PROBE_CANARY")

	// Write the canary first so the test can detect a vacuous green
	// (probe never ran). This write targets the writable workdir, so it
	// must succeed under any correct ruleset.
	if err := os.WriteFile(canaryPath, []byte("ran"), 0o644); err != nil {
		fmt.Println("canary write failed:", err)
		return 2
	}

	// Step 1: create a new directory entry for the read-only file inside
	// the writable workdir. A filesystem ruleset that withholds the refer
	// right from read-only grants denies this.
	if err := os.Link(roPath, linkPath); err != nil {
		fmt.Println("link denied:", err)
		return 0
	}
	// Step 2: if the new name was created, opening it for writing must
	// still not reach the original inode.
	f, err := os.OpenFile(linkPath, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		fmt.Println("open for write denied:", err)
		return 0
	}
	if _, err := f.WriteString("changed"); err != nil {
		f.Close()
		fmt.Println("write denied:", err)
		return 0
	}
	f.Close()
	fmt.Println("write-through succeeded: the read-only grant was modified via a workdir alias")
	return 1
}

// TestSecurityReadOnlyGrantResistsHardlinkModification is the
// end-to-end counterpart: a confined agent process that gives a
// read-only-granted file a new name in the workdir and writes through
// it must not be able to mutate the original. The assertion is made on
// the host-side original after the sandboxed process exits, so it
// catches the modification regardless of which syscall the ruleset
// denies.
//
// Like the probe test above, this runs stage2 directly (no bubblewrap
// mount namespace) to avoid bwrap's userns EXDEV on cross-mount link(2),
// which would mask the vulnerability.
func TestSecurityReadOnlyGrantResistsHardlinkModification(t *testing.T) {
	if abi := LandlockABI(); abi < 4 {
		t.Fatalf("Landlock ABI %d < 4: the filesystem ruleset requires ABI 4 (TRUNCATE/REFER); cannot verify read-only grant enforcement on this kernel — run in the e2e container", abi)
	}

	roRoot := t.TempDir()
	secret := filepath.Join(roRoot, "config")
	const original = "untouched"
	if err := os.WriteFile(secret, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	wd := t.TempDir()

	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Read: []string{roRoot}},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	omac := buildOmac(t)
	g.ReadPaths = append(g.ReadPaths, filepath.Dir(omac))

	linkPath := filepath.Join(wd, "alias")
	canary := filepath.Join(wd, "canary")
	// The agent gives the read-only file a new name in the workdir and
	// writes through it. Every step is best-effort (`|| true`); the
	// property is checked on the original afterwards, not on the
	// script's exit code. The canary write proves the script ran.
	script := fmt.Sprintf(`ln '%s' '%s' 2>/dev/null || true; printf changed > '%s' 2>/dev/null || true; echo ran > '%s'`,
		secret, linkPath, linkPath, canary)
	stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
	tail := append(append([]string{}, stage2...), "--", "/bin/sh", "-c", script)
	cmd := exec.Command(tail[0], tail[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("stage2 exited non-zero (%v): %s — a denial at any step also satisfies the property", err, out)
	}
	// The canary proves the script ran under stage2 enforcement. Without
	// it, a vacuous green (script never executed) would go undetected.
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("the agent script did not run (canary %q missing): the FS ruleset may have blocked /bin/sh from executing\n%s", canary, out)
	}
	if data, rerr := os.ReadFile(secret); rerr != nil || string(data) != original {
		t.Errorf("a confined process modified a read-only grant through a name it created in the workdir: content=%q err=%v", data, rerr)
	}
}
