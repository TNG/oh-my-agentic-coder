package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/session"
)

// runResume implements `omac resume [harness] [flags]`: list the recent
// sessions for the current workdir, let the user pick one in an interactive
// numbered picker, and launch it inside omac via the shared start pipeline
// with the harness's "resume by id" inner flag.
func runResume(args []string, env *Env) int {
	opts, ok := parseLaunchArgs("resume", args, env)
	if !ok {
		return ExitMisuse
	}
	h := opts.harness
	if h.Session == nil || h.Session.ResumeByIDArgs == nil {
		fmt.Fprintf(env.Stderr,
			"omac resume: harness %q does not support resuming sessions; try `omac continue %s`\n", h.Name, h.Name)
		return ExitOK
	}

	sessions, err := session.List(h, env.Workdir)
	if errors.Is(err, session.ErrUnsupported) {
		fmt.Fprintf(env.Stderr,
			"omac resume: listing sessions is not supported for harness %q; try `omac continue %s`\n", h.Name, h.Name)
		return ExitOK
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac resume: list sessions:", err)
		return ExitIOError
	}
	if len(sessions) == 0 {
		fmt.Fprintf(env.Stdout, "No resumable sessions for this directory (%s).\n", env.Workdir)
		return ExitOK
	}

	idx, ok := pickSession(env, h.Name, sessions)
	if !ok {
		return ExitOK // cancelled, or non-interactive stdin
	}

	opts.innerArgs = buildResumeInnerArgs(h.Session, sessions[idx].ID, opts.innerArgs)
	return runLaunch(env, opts)
}

// buildResumeInnerArgs puts the harness's resume-by-id flag first, then any
// user-supplied inner args.
func buildResumeInnerArgs(sess *config.HarnessSession, id string, userInner []string) []string {
	inner := append([]string(nil), sess.ResumeByIDArgs(id)...)
	return append(inner, userInner...)
}

// pickSession renders the session list and reads a 1-based selection from
// stdin. It returns the chosen 0-based index and true, or false when the user
// cancels (empty line / EOF / Ctrl-C), enters an invalid choice, or the
// session is not interactive (in which case the list is still printed with a
// hint). Interactivity requires BOTH stdin and stdout to be a TTY.
func pickSession(env *Env, harnessName string, sessions []session.Session) (int, bool) {
	st := newStyler(env.Stdout)
	renderSessions(env, st, harnessName, sessions)

	if !term.IsTerminal(int(env.Stdin.Fd())) || !term.IsTerminal(int(env.Stdout.Fd())) {
		fmt.Fprintln(env.Stderr,
			"\nomac resume: run in an interactive terminal to select a session "+
				"(or use `omac continue`).")
		return 0, false
	}

	fmt.Fprintf(env.Stdout, "\nSelect a session [1-%d] (Enter to cancel): ", len(sessions))

	// Treat Ctrl-C during the prompt as cancel (exit OK), not a process kill.
	// We intercept SIGINT only for the duration of the read; the default
	// handler is restored on return via signal.Stop.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	lineCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(env.Stdin).ReadString('\n')
		lineCh <- line
	}()

	select {
	case <-sigCh:
		fmt.Fprintln(env.Stdout) // finish the prompt line the ^C interrupted
		return 0, false
	case line := <-lineCh:
		idx, ok := parseSelection(line, len(sessions))
		if !ok && strings.TrimSpace(line) != "" {
			fmt.Fprintf(env.Stderr, "omac resume: invalid selection %q\n", strings.TrimSpace(line))
		}
		return idx, ok
	}
}

// parseSelection interprets a picker input line against a list of n items. It
// returns the 0-based index and true for a valid 1..n choice; an empty line
// (cancel) or any out-of-range/non-numeric input returns false.
func parseSelection(line string, n int) (int, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return 0, false
	}
	v, err := strconv.Atoi(line)
	if err != nil || v < 1 || v > n {
		return 0, false
	}
	return v - 1, true
}

// renderSessions prints a numbered, styled list of sessions (index, relative
// time, title) under a header naming the workdir and harness.
func renderSessions(env *Env, st styler, harnessName string, sessions []session.Session) {
	fmt.Fprintf(env.Stdout, "Recent sessions for %s (%s):\n\n",
		st.bold(env.Workdir), harnessName)
	// Column widths for alignment: index and (variable-length) session id.
	idxWidth := len(strconv.Itoa(len(sessions)))
	idWidth := 0
	for _, s := range sessions {
		if len(s.ID) > idWidth {
			idWidth = len(s.ID)
		}
	}
	for i, s := range sessions {
		num := fmt.Sprintf("%*d", idxWidth, i+1)
		when := fmt.Sprintf("%-8s", relativeTime(s.When))
		id := fmt.Sprintf("%-*s", idWidth, s.ID)
		fmt.Fprintf(env.Stdout, "  %s  %s  %s  %s\n",
			st.cyan(num), st.gray(when), st.gray(id), s.Title)
	}
}

// relativeTime renders t as a compact "time ago" string (e.g. "2h", "3d").
// A zero time renders as "unknown".
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
