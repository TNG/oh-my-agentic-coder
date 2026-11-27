package sandboxrun

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"golang.org/x/term"
)

// TerminalScanAsk asks on the terminal whether to continue a .env scan that
// hit its limit. It returns nil unless in and out are both terminals, so
// callers without one refuse the start instead.
func TerminalScanAsk(in *os.File, out io.Writer, profilePath string) ScanAsk {
	outFile, ok := out.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(outFile.Fd())) {
		return nil
	}
	return abortOnInterrupt(scanAskFrom(in, out, profilePath), out)
}

// abortOnInterrupt turns Ctrl-C while ask waits for an answer into an abort,
// so the caller's cleanup runs instead of the process dying on SIGINT. An
// interrupted read keeps waiting in the background; nothing asks again after
// an abort.
func abortOnInterrupt(ask ScanAsk, w io.Writer) ScanAsk {
	return func(roots []string, limit int64) ScanChoice {
		interrupted := make(chan os.Signal, 1)
		signal.Notify(interrupted, os.Interrupt)
		defer signal.Stop(interrupted)
		answer := make(chan ScanChoice, 1)
		go func() { answer <- ask(roots, limit) }()
		select {
		case choice := <-answer:
			return choice
		case <-interrupted:
			fmt.Fprintln(w)
			return ScanAbort
		}
	}
}

func scanAskFrom(r io.Reader, w io.Writer, profilePath string) ScanAsk {
	profile := profileName(profilePath)
	lines := bufio.NewReader(r)
	// The first question comes at the base limit, and each continue adds
	// that base limit again.
	var step int64
	return func(roots []string, limit int64) ScanChoice {
		if step == 0 {
			step = limit
		}
		fmt.Fprintf(w, `omac: the .env scan of %s stopped after %s entries.
  [c] continue scanning (next %s)
  [s] start with incomplete protection: .env files in the unscanned part stay readable
  [A] abort (default)
Tip: raise the limit or skip folders with filesystem.deny_scan in
     %s
`, strings.Join(roots, ", "), formatCount(limit), formatCount(step), profile)
		answer, _ := lines.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "c", "continue":
			return ScanContinue
		case "s", "start":
			return ScanStartIncomplete
		default:
			return ScanAbort
		}
	}
}
