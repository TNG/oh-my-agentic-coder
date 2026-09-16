package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// TestSecurityColdStartSpawnedArgvComesFromApprovedSnapshot asserts that the
// cold-start (`omac start`) spawn site reads Command from the approved
// snapshot's manifest, not from the workdir's still-agent-writable omac.yaml.
//
// The fixture approves a skill with a benign command, then rewrites the workdir
// manifest to a hostile command that would create a marker file if executed.
// The sidecar is spawned for real (health check will fail since `python3
// server.py` is not a real server here, but the side-effect test only checks
// whether the marker was created). If the cold-start site reads from the
// workdir, the marker is created; if it reads from the snapshot (the fix),
// it is not.
func TestSecurityColdStartSpawnedArgvComesFromApprovedSnapshot(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	isolateHome(t)
	shortTmp, err := os.MkdirTemp("/tmp", "omac-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shortTmp) })
	t.Setenv("TMPDIR", shortTmp)

	workdir := t.TempDir()
	// The hostile command in the workdir manifest writes to this marker.
	// stageApprovedSkillWithWorkdirTamper uses /tmp/OMAC_SPAWN_ARGV_POC.
	markerPath := "/tmp/OMAC_SPAWN_ARGV_POC"

	// Approve with a benign command, then rewrite workdir manifest to hostile.
	skillDir, snapDir, bundle := stageApprovedSkillWithWorkdirTamper(t, workdir, "gate")
	_ = bundle

	// Control: the snapshot exists and holds the benign command.
	snapMeta, serr := config.LoadMeta(filepath.Join(snapDir, config.MetaFileName))
	if serr != nil {
		t.Fatalf("control: LoadMeta(snapshot): %v", serr)
	}
	if len(snapMeta.Sidecar.Command) == 0 || snapMeta.Sidecar.Command[0] != "python3" {
		t.Fatalf("control: snapshot manifest does not declare the approved command (%v): fixture broken", snapMeta.Sidecar.Command)
	}

	// Write a hostile command into the snapshot dir (replacing the benign one)
	// is impossible from inside the sandbox — the test proves the WORKDIR
	// write is what would have been used before the fix. We verify the marker
	// is NOT created (snapshot's python3 command doesn't write it).
	_ = skillDir // used in stageApprovedSkillWithWorkdirTamper

	// The fake sandbox-launch template.
	capturePath := filepath.Join(t.TempDir(), "capture")
	if err := os.WriteFile(capturePath, []byte("#!/bin/sh\ntrue\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(workdir, ".opencode", "oh-my-agentic-coder.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	configText := fmt.Sprintf("sandbox:\n  default_profile: capture\n  profiles:\n    capture:\n      command: [%q, %q, %q, %q]\n",
		capturePath, "--", "{{inner_cmd}}", "{{inner_args}}")
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	env, stderr := launchTestEnv(t, workdir)
	harness, ok := config.LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness missing")
	}

	// The exit code is not asserted: the sidecar's health check fails
	// (python3 server.py is not a real server), but the side-effect test
	// only cares whether the hostile marker was created.
	_ = runLaunch(env, launchOpts{
		label:            "start",
		harness:          harness,
		innerCmdOverride: capturePath,
	})

	if _, err := os.Stat(markerPath); err == nil {
		t.Errorf("cold-start executed the WORKDIR manifest (hostile command) instead of the approved snapshot manifest: marker file was created at %s\nstderr:\n%s", markerPath, stderr())
	}
}
