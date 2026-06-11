// Package sandboxrun implements `omac sandbox run`: it resolves a
// sandboxprofile into a concrete grant set, starts the filtering
// proxy, and launches the inner command under the platform kernel
// sandbox (Seatbelt via sandbox-exec on macOS, bubblewrap + Landlock
// on Linux).
package sandboxrun

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// Grants is the fully resolved, expanded, existence-filtered input to
// the platform backends.
type Grants struct {
	Workdir string // absolute; always the child's cwd

	// Path grants (expanded, absolute, existing).
	ReadPaths  []string // read-only
	WritePaths []string // write-only
	AllowPaths []string // read+write

	// ProtectedPaths are denied even under broader grants (expanded;
	// override_deny holes already punched). Not existence-filtered:
	// a missing ~/.ssh today may exist tomorrow.
	ProtectedPaths []string

	// Network.
	NetworkMode     string // filtered|blocked|open
	ProxyPort       int    // 0 when no proxy is running
	ListenPorts     []int
	AllowTCPConnect []int
	OpenPorts       []int

	// UnixSockets lists socket files granted via --allow-file that
	// need explicit AF_UNIX connect allowance on macOS.
	UnixSockets []string

	Enforcement string // kernel|env-only
}

// ResolveGrants merges the profile, the platform baseline, and the
// workdir access level into a Grants value. notices receives skip
// notices for nonexistent paths.
func ResolveGrants(p *sandboxprofile.Profile, workdir string, notices io.Writer) (*Grants, error) {
	base := sandboxprofile.PlatformBaseline()

	read, err := sandboxprofile.ExpandExisting(append(append([]string{}, base.Read...), p.Filesystem.Read...), notices)
	if err != nil {
		return nil, err
	}
	write, err := sandboxprofile.ExpandExisting(append(append([]string{}, base.Write...), p.Filesystem.Write...), notices)
	if err != nil {
		return nil, err
	}
	allow, err := sandboxprofile.ExpandExisting(p.Filesystem.Allow, notices)
	if err != nil {
		return nil, err
	}

	// Workdir grant.
	switch p.Workdir.Access {
	case sandboxprofile.AccessRead:
		read = append(read, workdir)
	case sandboxprofile.AccessWrite:
		write = append(write, workdir)
	case sandboxprofile.AccessReadWrite:
		allow = append(allow, workdir)
	}

	g := &Grants{
		Workdir:         workdir,
		ReadPaths:       dedupe(read),
		WritePaths:      dedupe(write),
		AllowPaths:      dedupe(allow),
		ProtectedPaths:  sandboxprofile.EffectiveProtectedPaths(base, p.Filesystem.OverrideDeny),
		NetworkMode:     p.Network.EffectiveMode(),
		ListenPorts:     dedupeInts(p.Network.ListenPort),
		AllowTCPConnect: dedupeInts(p.Network.AllowTCPConnect),
		OpenPorts:       dedupeInts(p.Network.OpenPort),
		Enforcement:     p.Network.EffectiveEnforcement(),
	}

	// Unix sockets: any allow-granted path that is a socket file gets
	// the macOS AF_UNIX carve-out.
	for _, path := range g.AllowPaths {
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			g.UnixSockets = append(g.UnixSockets, path)
		}
	}
	return g, nil
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func dedupeInts(in []int) []int {
	seen := make(map[int]bool, len(in))
	var out []int
	for _, n := range in {
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// withUnrestrictedFilesystem returns a copy of the grants with the
// filesystem opened up (learn mode): the root directory becomes a
// read+write grant and the protected-path denials are dropped.
// Network and env restrictions are untouched.
func (g *Grants) withUnrestrictedFilesystem() *Grants {
	out := *g
	out.AllowPaths = append(append([]string{}, g.AllowPaths...), "/")
	out.ProtectedPaths = nil
	return &out
}

// Validate sanity-checks the grant set before launch.
func (g *Grants) Validate() error {
	if g.Workdir == "" {
		return fmt.Errorf("sandbox grants: empty workdir")
	}
	switch g.NetworkMode {
	case sandboxprofile.ModeFiltered, sandboxprofile.ModeBlocked, sandboxprofile.ModeOpen:
	default:
		return fmt.Errorf("sandbox grants: invalid network mode %q", g.NetworkMode)
	}
	return nil
}
