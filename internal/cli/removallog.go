package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
)

const removalLogFilename = "skill-removals.json"
const acknowledgedFilename = "acknowledged-removals.json"

type removalLog struct {
	Version  int               `json:"version"`
	Removals map[string]string `json:"removals"` // skill name → RFC3339 removal timestamp
}

type acknowledgedRemovals struct {
	Version      int               `json:"version"`
	Acknowledged map[string]string `json:"acknowledged"` // skill name → RFC3339 acknowledgement timestamp
}

func globalRemovalLogPath() string {
	dir := registry.GlobalDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, removalLogFilename)
}

func workdirAcknowledgedPath(workdir string) string {
	return filepath.Join(workdir, ".opencode", acknowledgedFilename)
}

// recordGlobalRemoval records a skill removal in the global tombstone log.
// Called by omac uninstall when a user-global skill is removed so that every
// other project can warn once on its next omac start.
func recordGlobalRemoval(name string) error {
	path := globalRemovalLogPath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	log := &removalLog{Version: 1, Removals: map[string]string{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, log)
		if log.Removals == nil {
			log.Removals = map[string]string{}
		}
	}
	log.Removals[name] = time.Now().UTC().Format(time.RFC3339)
	return writeRemovalLogJSON(path, log)
}

// warnUnacknowledgedRemovals compares the global removal log against a
// per-workdir acknowledgement file. For every globally-uninstalled skill
// not yet seen by this project it prints a one-time warning and marks it
// acknowledged so subsequent omac start runs are silent.
func warnUnacknowledgedRemovals(env *Env) {
	logPath := globalRemovalLogPath()
	if logPath == "" {
		return
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return // no removals on record
	}
	var log removalLog
	if err := json.Unmarshal(data, &log); err != nil || len(log.Removals) == 0 {
		return
	}

	ackPath := workdirAcknowledgedPath(env.Workdir)
	ack := &acknowledgedRemovals{Version: 1, Acknowledged: map[string]string{}}
	if data, err := os.ReadFile(ackPath); err == nil {
		_ = json.Unmarshal(data, ack)
		if ack.Acknowledged == nil {
			ack.Acknowledged = map[string]string{}
		}
	}

	var warned bool
	for name, removedAt := range log.Removals {
		if _, seen := ack.Acknowledged[name]; seen {
			continue
		}
		fmt.Fprintf(env.Stderr,
			"[warn] global skill %q was uninstalled on %s and is no longer available.\n"+
				"       Run `omac list` to see what is currently installed.\n",
			name, removedAt)
		ack.Acknowledged[name] = time.Now().UTC().Format(time.RFC3339)
		warned = true
	}
	if !warned {
		return
	}
	if err := os.MkdirAll(filepath.Dir(ackPath), 0o700); err != nil {
		return
	}
	_ = writeRemovalLogJSON(ackPath, ack)
}

func writeRemovalLogJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
