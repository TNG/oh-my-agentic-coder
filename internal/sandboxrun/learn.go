package sandboxrun

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// learnSampleInterval is how often the recorder samples open fds.
const learnSampleInterval = 2 * time.Second

// learnRecorder samples the open file descriptors of the sandboxed
// process group while a learn-mode session runs and aggregates the
// directories seen. fd sampling is approximate (short-lived opens can
// be missed) — learn mode is a convenience for building the
// allowlist, not an enforcement mechanism.
type learnRecorder struct {
	mu      sync.Mutex
	seen    map[string]bool // directories observed
	stop    chan struct{}
	stopped sync.WaitGroup

	excluded  []string // expanded: granted + baseline-read/write roots
	protected []string // expanded protected paths (never offered)
	home      string
}

// newLearnRecorder builds the exclusion sets from the effective grants.
func newLearnRecorder(g *Grants) *learnRecorder {
	home, _ := os.UserHomeDir()
	r := &learnRecorder{
		seen:      map[string]bool{},
		stop:      make(chan struct{}),
		protected: g.ProtectedPaths,
		home:      home,
	}
	r.excluded = append(r.excluded, g.ReadPaths...)
	r.excluded = append(r.excluded, g.WritePaths...)
	r.excluded = append(r.excluded, g.AllowPaths...)
	r.excluded = append(r.excluded, g.Workdir)
	// Never offer temp dirs or the omac state dir.
	for _, p := range []string{os.TempDir(), "/tmp", "/private/tmp", "/private/var/folders", "/var/folders"} {
		r.excluded = append(r.excluded, p)
	}
	if home != "" {
		r.excluded = append(r.excluded,
			filepath.Join(home, ".local", "state", "omac"),
			filepath.Join(home, ".config", "omac"))
	}
	return r
}

// Start begins sampling the process group of pid.
func (r *learnRecorder) Start(pid int) {
	r.stopped.Add(1)
	go func() {
		defer r.stopped.Done()
		ticker := time.NewTicker(learnSampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
				for _, dir := range sampleOpenDirs(pid) {
					r.record(dir)
				}
			}
		}
	}()
}

// Stop ends sampling and returns the aggregated candidate folders.
func (r *learnRecorder) Stop() []string {
	close(r.stop)
	r.stopped.Wait()
	return r.candidates()
}

// record notes one observed path (file or dir; files are reduced to
// their directory).
func (r *learnRecorder) record(path string) {
	if path == "" || !filepath.IsAbs(path) {
		return
	}
	r.mu.Lock()
	r.seen[path] = true
	r.mu.Unlock()
}

// candidates aggregates the raw observations: exclude granted/
// baseline/temp/protected paths, reduce to project-level directories,
// collapse descendants into ancestors.
func (r *learnRecorder) candidates() []string {
	r.mu.Lock()
	raw := make([]string, 0, len(r.seen))
	for p := range r.seen {
		raw = append(raw, p)
	}
	r.mu.Unlock()

	var kept []string
	for _, p := range raw {
		if r.isExcluded(p) || r.isProtected(p) {
			continue
		}
		kept = append(kept, r.projectRoot(p))
	}
	return collapseAncestors(dedupe(kept))
}

func (r *learnRecorder) isExcluded(path string) bool {
	for _, ex := range r.excluded {
		if path == ex || strings.HasPrefix(path, ex+string(filepath.Separator)) {
			return true
		}
	}
	// System trees are baseline-granted in restricted mode; never offer them.
	for _, sys := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/System", "/Library", "/opt", "/Applications", "/dev", "/proc", "/sys", "/run", "/private/etc", "/private/var"} {
		if path == sys || strings.HasPrefix(path, sys+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (r *learnRecorder) isProtected(path string) bool {
	for _, prot := range r.protected {
		if path == prot || strings.HasPrefix(path, prot+string(filepath.Separator)) ||
			strings.HasPrefix(prot, path+string(filepath.Separator)) {
			// Also reject ancestors of protected paths: offering
			// /Users/u would implicitly cover ~/.ssh.
			if strings.HasPrefix(prot, path+string(filepath.Separator)) {
				return true
			}
			return true
		}
	}
	return false
}

// projectRoot reduces an observed path to a sensible grant root:
// directories directly under $HOME (or under two levels of grouping
// like ~/Files/projects/<name>) map to the deepest directory that is
// at most three levels below $HOME; everything else maps to itself.
func (r *learnRecorder) projectRoot(path string) string {
	if r.home == "" {
		return path
	}
	rel, err := filepath.Rel(r.home, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	parts := strings.Split(rel, string(filepath.Separator))
	// Keep at most 3 components below home (~/Files/projects/<name>).
	if len(parts) > 3 {
		parts = parts[:3]
	}
	return filepath.Join(append([]string{r.home}, parts...)...)
}

// collapseAncestors removes entries already covered by another entry.
func collapseAncestors(paths []string) []string {
	sort.Strings(paths)
	var out []string
	for _, p := range paths {
		covered := false
		for _, kept := range out {
			if p == kept || strings.HasPrefix(p, kept+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, p)
		}
	}
	return out
}

// OfferLearnedFolders presents the candidates on the terminal and asks
// whether to append them to the profile's filesystem.allow list. It
// rewrites the profile pretty-printed on confirmation. in/out default
// to the controlling terminal so the prompt works after a TUI session.
func OfferLearnedFolders(profilePath string, candidates []string, in io.Reader, out io.Writer) error {
	if len(candidates) == 0 {
		fmt.Fprintln(out, "omac sandbox: learn mode: no new folders observed")
		return nil
	}
	fmt.Fprintln(out, "\nomac sandbox: learn mode observed these folders outside the current profile:")
	for _, c := range candidates {
		fmt.Fprintf(out, "  %s\n", c)
	}
	fmt.Fprintf(out, "Add them to filesystem.allow in %s? [y/N] ", profilePath)
	reader := bufio.NewReader(in)
	answer, _ := reader.ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "y" && answer != "yes" {
		fmt.Fprintln(out, "omac sandbox: profile unchanged")
		return nil
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return fmt.Errorf("learn mode: read profile: %w", err)
	}
	profile, err := sandboxprofile.Parse(data)
	if err != nil {
		return fmt.Errorf("learn mode: %w", err)
	}
	existing := map[string]bool{}
	for _, a := range profile.Filesystem.Allow {
		existing[a] = true
	}
	added := 0
	for _, c := range candidates {
		entry := abbreviateHome(c)
		if existing[entry] {
			continue
		}
		profile.Filesystem.Allow = append(profile.Filesystem.Allow, entry)
		added++
	}
	if added == 0 {
		fmt.Fprintln(out, "omac sandbox: all folders already present; profile unchanged")
		return nil
	}
	if err := sandboxprofile.WriteProfile(profilePath, profile); err != nil {
		return fmt.Errorf("learn mode: write profile: %w", err)
	}
	fmt.Fprintf(out, "omac sandbox: added %d folder(s) to %s\n", added, profilePath)
	return nil
}

// abbreviateHome renders /Users/u/x as ~/x for nicer profile entries.
func abbreviateHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}
