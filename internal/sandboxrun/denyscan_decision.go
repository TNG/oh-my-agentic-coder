package sandboxrun

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// ScanChoice is the user's answer when the .env scan of a folder hits its
// entry limit.
type ScanChoice int

const (
	ScanAbort ScanChoice = iota
	ScanContinue
	ScanStartIncomplete
)

// ScanAsk asks what to do about roots whose scan stopped at limit entries.
// A nil ScanAsk means there is no terminal to ask on.
type ScanAsk func(roots []string, limit int64) ScanChoice

var errScanAborted = errors.New("the .env scan was aborted, so nothing was started")

// ProtectedScan is the result of ScanProtected: what the .env scan found and
// what the user decided for folders that hit the limit.
type ProtectedScan struct {
	Matches     []string // union over all passes, deduplicated and sorted
	ByNameRoots []string // too large to scan, covered by the macOS name rule
	Accepted    []string // roots the user started with incomplete protection
	Limit       int64    // effective limit after every continue
	Continued   bool
	Entries     map[string]int64 // entries counted per root in the last pass

	asked       []string // roots that hit the limit in any pass
	profilePath string
	cache       *denyScanCache
	roots       []string
	protected   []string
	params      denyScanParams
}

// ScanProtected scans the folders the sandbox will grant for protected files
// such as .env. When a folder hits the entry limit and the macOS name rule
// cannot cover it, ask decides: continue rescans with the limit raised by the
// profile's base limit, reusing the cache so folders read before cost one
// lstat each; a cached folder still counts against the limit, which is why
// the limit has to grow. Each pass reports only matches from folders it
// re-read, so the matches of all passes are joined.
func ScanProtected(p *sandboxprofile.Profile, workdir, profilePath string, ask ScanAsk) (*ProtectedScan, error) {
	base := sandboxprofile.PlatformBaseline()
	roots, err := denyScanRoots(p, workdir)
	if err != nil {
		return nil, err
	}
	protected := sandboxprofile.EffectiveProtectedPaths(base, p.Filesystem.OverrideDeny)
	protected = append(protected, literalDenies(p)...)
	globs := denyGlobs(p.Filesystem.Deny, base.WorkdirProtected, p.Filesystem.OverrideDeny)

	s := &ProtectedScan{
		profilePath: profilePath,
		cache:       newDenyScanCache(),
		roots:       roots,
		protected:   protected,
		params:      denyScanParamsFor(p),
	}
	matches := map[string]bool{}
	for {
		out, byName, incomplete, scans, err := resolveDenyScans(p.Filesystem.Deny, base.WorkdirProtected, p.Filesystem.OverrideDeny, roots, protected, s.params, nil, s.cache, denyScanConcurrency)
		if err != nil {
			return nil, err
		}
		for _, m := range out {
			matches[m] = true
		}
		s.ByNameRoots = byName.roots
		s.Entries = make(map[string]int64, len(scans))
		for _, scan := range scans {
			s.Entries[scan.root] = scan.entries
		}
		if len(incomplete) == 0 {
			break
		}
		if ask == nil {
			e := newIncompleteScanError(incomplete, s.params.limit, false, globs, p.Filesystem.OverrideDeny)
			e.ProfilePath = profilePath
			return nil, e
		}
		var stopped []string
		for _, scan := range incomplete {
			stopped = append(stopped, scan.root)
			if !slices.Contains(s.asked, scan.root) {
				s.asked = append(s.asked, scan.root)
			}
		}
		choice := ask(stopped, s.params.limit)
		if choice == ScanStartIncomplete {
			s.Accepted = stopped
			break
		}
		if choice != ScanContinue {
			return nil, errScanAborted
		}
		s.Continued = true
		s.params.limit += p.DenyScanBaseLimit()
	}
	s.Limit = s.params.limit
	s.Matches = slices.Sorted(maps.Keys(matches))
	return s, nil
}

// SandboxFlags passes the scan decision to `omac sandbox run`, so its own scan
// uses the same limit and does not ask again.
func (s *ProtectedScan) SandboxFlags() []string {
	flags := []string{"--deny-scan-limit", strconv.FormatInt(s.Limit, 10)}
	for _, root := range s.Accepted {
		flags = append(flags, "--deny-scan-accept-incomplete", root)
	}
	return flags
}

// SessionEndNotes are printed after the session: the max_entries value that
// avoids the question next time for every folder that finished after a
// continue, and which folders were started with incomplete protection.
func (s *ProtectedScan) SessionEndNotes(profilePath string) []string {
	profile := profilePath
	if profile == "" {
		profile = "your sandbox profile"
	}
	var notes []string
	if s.Continued {
		for _, root := range s.asked {
			if slices.Contains(s.Accepted, root) {
				continue
			}
			needed := s.Entries[root]
			notes = append(notes, fmt.Sprintf("note: the .env scan of %s needed %s entries.\nSet filesystem.deny_scan.max_entries to %d in %s to skip the question next time.",
				root, formatCount(needed), roundUpToMillion(needed), profile))
		}
	}
	if len(s.Accepted) > 0 {
		notes = append(notes, "note: these folders were not fully protected this session: "+strings.Join(s.Accepted, ", "))
	}
	return notes
}

func roundUpToMillion(n int64) int64 {
	const million = 1_000_000
	return (n + million - 1) / million * million
}
