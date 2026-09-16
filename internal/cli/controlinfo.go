package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/registry"
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
//
// ControlToken is the per-session bearer token required on all control-plane
// requests. It is written here so host-side CLI commands (omac register, etc.)
// can notify the running serve without knowing the token out-of-band. The file
// lives in ~/.config/omac/ which the sandbox baseline never mounts, so a
// confined agent cannot read it.
type controlInfo struct {
	ControlBase  string `json:"control_base"`
	ControlToken string `json:"control_token"`
	PID          int    `json:"pid"`
	StartedAt    string `json:"started_at"`
}

// controlInfoPath returns the well-known path of the serve control-info file,
// or "" when the user config dir cannot be resolved. The file lives in
// ~/.config/omac, which the sandbox baseline protects and never mounts, so a
// confined agent cannot read or rewrite it.
func controlInfoPath() string {
	dir := registry.GlobalDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "serve-control.json")
}

// writeControlInfo publishes the running serve's control URL and token.
// Best-effort: a write failure is logged by the caller but does not abort serve.
func writeControlInfo(controlBase, controlToken string) error {
	p := controlInfoPath()
	if p == "" {
		return fmt.Errorf("control-info: no user config dir available; refusing to write to shared temp")
	}
	ci := controlInfo{
		ControlBase:  controlBase,
		ControlToken: controlToken,
		PID:          os.Getpid(),
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(ci, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	// O_EXCL|O_NOFOLLOW: fail if a symlink is already in place at the staging
	// path so an attacker cannot redirect the write into another file.
	// Remove any leftover staging file first (e.g. from a previous crash).
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}

// removeControlInfo deletes the control-info file, but only if it still
// belongs to this process (so a stale file from a crashed predecessor that
// a new serve already overwrote isn't clobbered on our exit).
func removeControlInfo() {
	p := controlInfoPath()
	if p == "" {
		return
	}
	ci, ok := readControlInfo()
	if ok && ci.PID == os.Getpid() {
		_ = os.Remove(p)
	}
}

// readControlInfo loads and validates the control-info file.
// ok=false when the file is absent, unparsable, names a dead or foreign
// process, or points to a non-loopback address.
func readControlInfo() (controlInfo, bool) {
	p := controlInfoPath()
	if p == "" {
		return controlInfo{}, false
	}
	data, err := os.ReadFile(p)
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
	if !isLoopbackURL(ci.ControlBase) {
		return controlInfo{}, false
	}
	if !pidLiveAndOwned(ci.PID) {
		return controlInfo{}, false
	}
	return ci, true
}

// isLoopbackURL returns true when the host in rawURL is a loopback address.
func isLoopbackURL(rawURL string) bool {
	// Trim scheme so net.SplitHostPort works on both http://host:port and host:port.
	host := rawURL
	for _, pfx := range []string{"http://", "https://"} {
		if len(rawURL) > len(pfx) && rawURL[:len(pfx)] == pfx {
			host = rawURL[len(pfx):]
			break
		}
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		// No port — treat bare host.
		h = host
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
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
	if ci.ControlToken != "" {
		req.Header.Set("X-Omac-Control-Token", ci.ControlToken)
	}
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

// notifyReloadGlobal best-effort asks a running `omac serve` to re-activate
// its user-global skill layer, so a global skill just registered/deregistered
// is picked up without restarting serve. Same contract as notifyReload.
func notifyReloadGlobal() (bool, string) {
	ci, ok := readControlInfo()
	if !ok {
		return false, "no running omac serve detected"
	}
	req, err := http.NewRequest(http.MethodPost, ci.ControlBase+"/__omac__/reload-global", nil)
	if err != nil {
		return false, "reload-global request build failed"
	}
	if ci.ControlToken != "" {
		req.Header.Set("X-Omac-Control-Token", ci.ControlToken)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Sprintf("omac serve not reachable at %s (stale control file?)", ci.ControlBase)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, "reloaded global skills in running omac serve"
	}
	return false, fmt.Sprintf("omac serve reload-global returned %d", resp.StatusCode)
}
