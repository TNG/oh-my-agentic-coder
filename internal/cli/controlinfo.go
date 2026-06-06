package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// controlInfo is the small record a running `omac serve` publishes so that
// other omac CLI invocations (register, deregister, secrets, config) can
// find it and ask it to reload a directory after they change on-disk state.
//
// It lives at a single well-known path (see controlInfoPath) rather than in
// the per-server runtime dir, because the CLI commands run with an arbitrary
// --workdir and cannot derive the server's runtime-dir hash. The normal
// deployment has one serve process; if that ever needs to change this can
// grow into a directory of files keyed by pid.
type controlInfo struct {
	ControlBase string `json:"control_base"`
	PID         int    `json:"pid"`
	StartedAt   string `json:"started_at"`
}

// controlInfoPath returns the well-known path of the serve control-info file.
func controlInfoPath() string {
	return filepath.Join(os.TempDir(), "omac-serve-control.json")
}

// writeControlInfo publishes the running serve's control URL. Best-effort:
// a write failure is logged by the caller but does not abort serve.
func writeControlInfo(controlBase string) error {
	ci := controlInfo{
		ControlBase: controlBase,
		PID:         os.Getpid(),
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(ci, "", "  ")
	if err != nil {
		return err
	}
	tmp := controlInfoPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, controlInfoPath())
}

// removeControlInfo deletes the control-info file, but only if it still
// belongs to this process (so a stale file from a crashed predecessor that
// a new serve already overwrote isn't clobbered on our exit).
func removeControlInfo() {
	ci, ok := readControlInfo()
	if ok && ci.PID == os.Getpid() {
		_ = os.Remove(controlInfoPath())
	}
}

// readControlInfo loads the control-info file. ok=false when the file is
// absent or unparyable (no running serve, or a corrupt file).
func readControlInfo() (controlInfo, bool) {
	data, err := os.ReadFile(controlInfoPath())
	if err != nil {
		return controlInfo{}, false
	}
	var ci controlInfo
	if err := json.Unmarshal(data, &ci); err != nil {
		return controlInfo{}, false
	}
	if ci.ControlBase == "" {
		return controlInfo{}, false
	}
	return ci, true
}

// notifyReload best-effort asks a running `omac serve` to reload the given
// absolute directory, so a skill just installed/registered/edited there is
// picked up without restarting serve. It is a no-op (returns false) when no
// serve process is running. Errors are swallowed and surfaced only via the
// boolean + an optional caller message; this must never fail a CLI command
// whose primary on-disk work already succeeded.
//
// Returns (notified, reason): notified=true means the reload POST returned
// 2xx; reason carries a short human-readable status either way.
func notifyReload(absDir string) (bool, string) {
	ci, ok := readControlInfo()
	if !ok {
		return false, "no running omac serve detected"
	}
	body, _ := json.Marshal(map[string]string{"dir": absDir})
	req, err := http.NewRequest(http.MethodPost, ci.ControlBase+"/__omac__/reload", bytes.NewReader(body))
	if err != nil {
		return false, "reload request build failed"
	}
	req.Header.Set("content-type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Most likely the serve process exited and left a stale file.
		return false, fmt.Sprintf("omac serve not reachable at %s (stale control file?)", ci.ControlBase)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, fmt.Sprintf("reloaded %s in running omac serve", absDir)
	}
	if resp.StatusCode == http.StatusBadRequest {
		// e.g. dir not under the server's --root, or not a directory.
		return false, fmt.Sprintf("omac serve declined reload (%d) — dir may be outside the server's --root", resp.StatusCode)
	}
	return false, fmt.Sprintf("omac serve reload returned %d", resp.StatusCode)
}
