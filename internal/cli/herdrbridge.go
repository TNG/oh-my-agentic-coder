package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/herdr"
	"github.com/TNG/oh-my-agentic-coder/internal/session"
)

// maybeHerdrBridge builds the host-side herdr pane bridge for a launch,
// or nil when it must stay off:
//
//   - disabled via the global launcher config (`herdr.enabled: false`),
//   - omac is not running inside a herdr pane (no HERDR_* env),
//   - the harness has no host-side watchable session store, or no
//     resume-by-id support (the bridge would degrade herdr's own screen
//     state detection without adding a resume command in return).
//
// The bridge runs in the omac process, OUTSIDE the sandbox: it sees the
// host environment herdr sets for the pane, while the inner harness
// keeps its stripped environment. Nothing herdr-related is passed into
// the sandbox — see the package comment in internal/herdr for why that
// must stay true (the herdr socket API can drive other panes).
func maybeHerdrBridge(lc config.LauncherConfig, harness config.Harness, env *Env, reloader *startReloader, sessionID string, verbose bool) *herdr.Bridge {
	if !lc.Herdr.BridgeEnabled() {
		return nil
	}
	if harness.Session == nil || harness.Session.ResumeByIDArgs == nil {
		return nil
	}
	det, ok := herdr.Detect(os.Getenv)
	if !ok {
		return nil
	}
	logf := func(string, ...any) {}
	if verbose {
		logf = func(format string, args ...any) {
			fmt.Fprintf(env.Stderr, "[verbose] herdr: "+format+"\n", args...)
		}
	}
	probe := herdrProbe(harness, env.Workdir)
	if probe == nil {
		logf("no watchable session store for harness %q; bridge off", harness.Name)
		return nil
	}

	// Session-id resolution, most authoritative first: an explicit
	// resume (omac continue -s / omac resume) knows its id up front;
	// opencode reports the live session id through omac's control
	// plane during the run; a claude-code fresh session is discovered
	// by the probe itself (the new store file's name IS the id and is
	// surfaced by the bridge without this callback).
	sessionIDFn := func() string {
		if sessionID != "" {
			return sessionID
		}
		if harness.Session.ListKind == config.SessionListOpenCodeCLI && reloader != nil {
			return reloader.reportedSession()
		}
		return ""
	}

	logf("reporting pane %s to herdr (harness %s)", det.PaneID, harness.Name)
	return herdr.New(det, herdr.Options{
		Agent:      "omac",
		Probe:      probe,
		SessionID:  sessionIDFn,
		ResumeArgv: func(id string) []string { return herdrResumeArgv(harness, id) },
		Logf:       logf,
	})
}

// herdrProbe builds the store-activity probe for a harness. It returns
// nil for harnesses omac cannot watch from the host side. Watching the
// store files (not parsing them) keeps the poll cheap: a handful of
// stat calls every two seconds.
func herdrProbe(harness config.Harness, workdir string) herdr.Probe {
	if harness.Session == nil {
		return nil
	}
	switch harness.Session.ListKind {
	case config.SessionListOpenCodeCLI:
		// opencode keeps sessions in a sqlite db; in WAL mode the
		// companion -wal file carries the writes, so both are watched.
		db := session.OpenCodeDBPath()
		if db == "" {
			return nil
		}
		return herdr.NewFileProbe(db, db+"-wal", db+"-shm")
	case config.SessionListClaudeFiles:
		// claude-code appends one <id>.jsonl per session in the
		// project's encoded directory; new files surface their id.
		dir := session.ClaudeProjectDir(harness, workdir)
		if dir == "" {
			return nil
		}
		probe := herdr.NewFileProbe(dir)
		probe.DirIDs(dir, herdr.IDsFromJSONL)
		return probe
	default:
		return nil
	}
}

// herdrResumeArgv builds the command herdr runs to restore this pane
// after a server restart: `omac continue [harness] -s <id>`. Restoring
// through omac (rather than herdr's built-in `opencode --session` /
// `claude --resume`) is the point of the bridge: the session comes back
// inside the sandbox, with all launch-time checks re-applied. The first
// word must be a plain command on the user's PATH, so omac must be
// installed as `omac` for restore to work.
func herdrResumeArgv(harness config.Harness, id string) []string {
	if harness.Session == nil || harness.Session.ResumeByIDArgs == nil {
		return nil
	}
	if !herdr.ValidSessionID(id) {
		return nil
	}
	argv := []string{"omac", "continue"}
	if tok := strings.TrimSpace(continueHintToken(harness)); tok != "" {
		argv = append(argv, tok)
	}
	return append(argv, "-s", id)
}
