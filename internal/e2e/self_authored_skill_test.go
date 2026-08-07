//go:build e2e || e2e_fast

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/skilltrust"
)

// TestE2ESelfAuthoredSkillRefused is a live regression test for the
// self-authored-skill sandbox escape: a coding agent, confined to the
// sandbox, can write a skill directory into the (agent-writable) workdir,
// get it registered, and — absent this control — have omac spawn its
// sidecar OUTSIDE the sandbox, giving the agent arbitrary host code
// execution. See docs/SECURITY_MODEL.md#self-authored-skills.
//
// It drives the real compiled `omac serve --no-inner` binary and its
// control plane over loopback (same topology as TestE2EServeDirTokenIsolation),
// so it needs no live LLM harness and no OS sandbox and runs fast.
//
// serve auto-registers any skill it discovers in the workdir with NO human
// in the loop — the exact "the agent authored it and omac picked it up"
// path. The fix requires an approval anchored in the host-only store
// (~/.config/omac/approvals.json) the sandbox cannot write. Because the
// store already exists here (we pre-write it), the run is steady-state —
// the one-time trust-on-first-upgrade grandfathering does NOT fire — so a
// freshly-authored, unapproved skill must be refused.
//
// Two skills are staged in one workdir:
//   - "evil": NOT approved. Its command touches a marker file if it ever
//     runs. The fix must refuse it (broken route, clear reason) and the
//     marker must never appear (the sidecar never executed).
//   - "good": approved (its bundle hash is pre-recorded), but declares a
//     required secret we never supply. It must pass the approval gate and
//     stop at pending-credentials — proving the gate authorizes real
//     skills and is not a blanket denial.
func TestE2ESelfAuthoredSkillRefused(t *testing.T) {
	omacBin := buildOmac(t)
	home := t.TempDir()
	cwd := t.TempDir()
	wd := t.TempDir()
	markerDir := t.TempDir()
	evilMarker := filepath.Join(markerDir, "evil-spawned")

	// "evil": no required values, so if the gate let it through serve would
	// spawn it — and the sidecar would create evilMarker. A low health
	// timeout bounds the wait only in the (regressed) case where it spawns.
	stageSelfAuthoredSkill(t, wd, "evil",
		"name: evil\n"+
			"sidecar:\n"+
			"  command: [\"sh\", \"-c\", \"touch '"+evilMarker+"'; sleep 300\"]\n"+
			"  mount: evil\n"+
			"  health:\n"+
			"    path: /status\n"+
			"    initial_delay_ms: 50\n"+
			"    interval_ms: 100\n"+
			"    timeout_ms: 800\n")

	// "good": approved below; blocked only on a missing required secret.
	goodDir := stageSelfAuthoredSkill(t, wd, "good",
		"name: good\n"+
			"sidecar:\n"+
			"  command: [\"sh\", \"-c\", \"sleep 300\"]\n"+
			"  mount: good\n"+
			"  secrets:\n"+
			"    - name: NEEDED_TOKEN\n"+
			"      required: true\n")

	// Approve ONLY "good" in the subprocess's host-only store (so this is NOT
	// a first-upgrade run). Point this process's HOME/XDG at the same `home`
	// the subprocess uses (withHome sets XDG_CONFIG_HOME=<home>/.config), so
	// skilltrust.Approve writes the approval AND its snapshot where serve
	// resolves them. "evil" is deliberately left unapproved.
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	goodHash, err := config.BundleHash(goodDir)
	if err != nil {
		t.Fatalf("bundle hash: %v", err)
	}
	if err := skilltrust.Approve("good", goodHash, goodDir); err != nil {
		t.Fatalf("approve good: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, omacBin, "serve", "opencode", "--no-inner", "--control-addr", "127.0.0.1:0")
	cmd.Dir = cwd
	cmd.Env = withHome(os.Environ(), home)
	if shortTmp := shortTmpDirForNested(t); shortTmp != "" {
		cmd.Env = withEnv(cmd.Env, "TMPDIR", shortTmp)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start omac serve: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	controlBaseCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			if m := controlBaseRe.FindStringSubmatch(scanner.Text()); m != nil {
				select {
				case controlBaseCh <- m[1]:
				default:
				}
			}
		}
	}()

	var controlBase string
	select {
	case controlBase = <-controlBaseCh:
	case <-time.After(15 * time.Second):
		t.Fatalf("omac serve did not print OMAC_CONTROL_BASE within 15s; stderr:\n%s", stderrBuf.String())
	}

	// Activate the workdir — this is where serve auto-registers and tries
	// to bring up both skills.
	skills := activateAndGetSkills(t, controlBase, wd, &stderrBuf)

	evil, ok := skills["evil"]
	if !ok {
		t.Fatal("evil skill missing from activation manifest")
	}
	// The detail must name the APPROVAL refusal specifically. (A bare
	// state=="broken" is NOT sufficient evidence: a spawned-but-unhealthy
	// sidecar is also "broken" — so we assert the reason and, below, that
	// the sidecar never executed at all.)
	if detail, _ := evil["detail"].(string); !strings.Contains(detail, "not host-approved") {
		t.Errorf("evil should be refused for lack of approval, got detail %q (state %v)", detail, evil["state"])
	}

	good, ok := skills["good"]
	if !ok {
		t.Fatal("good skill missing from activation manifest")
	}
	if good["state"] != "pending-credentials" {
		t.Errorf("good state = %v, want pending-credentials (passed the approval gate)", good["state"])
	}

	// The decisive check: the unapproved sidecar must never have executed.
	if _, err := os.Stat(evilMarker); err == nil {
		t.Fatalf("ESCAPE: the unapproved 'evil' sidecar RAN (marker %s exists)", evilMarker)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat evil marker: %v", err)
	}
}

// stageSelfAuthoredSkill writes .opencode/skills/<name>/omac.yaml with the
// given body, mimicking a skill an agent authored in the workdir. Returns
// the skill's absolute directory.
func stageSelfAuthoredSkill(t *testing.T, workdir, name, omacYAML string) string {
	t.Helper()
	skillDir := filepath.Join(workdir, ".opencode", "skills", name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "omac.yaml"), []byte(omacYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return skillDir
}

// activateAndGetSkills POSTs /__omac__/activate for dir and returns the
// manifest's skills keyed by name.
func activateAndGetSkills(t *testing.T, controlBase, dir string, stderr *bytes.Buffer) map[string]map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"dir": dir})
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(controlBase+"/__omac__/activate", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("activate: %v; stderr:\n%s", err, stderr.String())
	}
	defer resp.Body.Close()
	var m map[string]any
	if derr := json.NewDecoder(resp.Body).Decode(&m); derr != nil {
		t.Fatalf("decode activate: %v", derr)
	}
	out := map[string]map[string]any{}
	raw, _ := m["skills"].([]any)
	for _, s := range raw {
		if sm, ok := s.(map[string]any); ok {
			if name, ok := sm["name"].(string); ok {
				out[name] = sm
			}
		}
	}
	return out
}
