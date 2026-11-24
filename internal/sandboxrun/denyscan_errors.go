package sandboxrun

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// IncompleteScanError reports that the .env scan of Roots stopped at Limit
// entries and the parent process did not approve an incomplete start, so the
// sandbox was not started. The caller adds the command prefix.
type IncompleteScanError struct {
	Roots           []string
	Limit           int64
	ProfilePath     string // "" renders as "your sandbox profile"
	GrewDuringStart bool
	// NameRuleNote says why the macOS name rule could not cover the folder;
	// empty when it does not apply.
	NameRuleNote string
}

func (e *IncompleteScanError) Error() string {
	roots := strings.Join(e.Roots, ", ")
	profile := profileName(e.ProfilePath)
	limit := formatCount(e.Limit)
	if e.GrewDuringStart {
		return fmt.Sprintf(`%s grew past %s entries while omac was starting
(files were added during the .env scan), so nothing was started.
Start again. If this keeps happening, raise "filesystem.deny_scan.max_entries"
in %s.`, roots, limit, profile)
	}
	return fmt.Sprintf(`the .env scan of %s stopped after %s entries%s.
omac cannot ask whether to continue because there is no terminal, so nothing was started.
Fix it in %s, then start again:
  - raise the limit:            "filesystem": {"deny_scan": {"max_entries": %s}}
  - or skip folders that hold no secrets, such as build output:
                                "filesystem": {"deny_scan": {"skip_dir_names": ["build-gradle"]}}
To find the right limit, run `+"`omac start`"+` in a terminal once and choose "continue";
omac prints the value it needed when the session ends.`,
		roots, limit, e.NameRuleNote, profile, strconv.FormatInt(2*e.Limit, 10))
}

func profileName(path string) string {
	if path == "" {
		return "your sandbox profile"
	}
	return path
}

func formatCount(n int64) string {
	digits := strconv.FormatInt(n, 10)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return digits
}

// splitAcceptedRoots separates the incomplete roots the parent process
// approved from the ones that must refuse the start.
func splitAcceptedRoots(incomplete []rootScan, accepted []string) (approved, refused []rootScan) {
	for _, scan := range incomplete {
		if rootListed(scan.root, accepted) {
			approved = append(approved, scan)
		} else {
			refused = append(refused, scan)
		}
	}
	return approved, refused
}

func rootListed(root string, list []string) bool {
	for _, rf := range pathForms(root) {
		for _, item := range list {
			if slices.Contains(pathForms(item), rf) {
				return true
			}
		}
	}
	return false
}

func newIncompleteScanError(refused []rootScan, limit int64, decided bool, globs, overrideDeny []string) *IncompleteScanError {
	e := &IncompleteScanError{Limit: limit, GrewDuringStart: decided}
	for _, scan := range refused {
		e.Roots = append(e.Roots, scan.root)
		if _, reason := canDenyByName(scan.root, globs, overrideDeny); e.NameRuleNote == "" {
			e.NameRuleNote = reason
		}
	}
	return e
}
