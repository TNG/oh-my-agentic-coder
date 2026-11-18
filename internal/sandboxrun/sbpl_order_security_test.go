package sandboxrun

import (
	"fmt"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestSecurityNoAllowAfterProtectedDenies guards the rule that a protected
// path is closed to every operation that can name a path and that no later
// rule can reopen it. Seatbelt is last-match-wins, so any allow emitted after
// a deny may override it: (allow network*) in open mode once let a sandboxed
// process connect to Unix sockets inside protected folders (docker.sock in
// ~/.docker, the GnuPG agent in ~/.gnupg).
func TestSecurityNoAllowAfterProtectedDenies(t *testing.T) {
	const (
		home      = "/Users/u"
		protected = home + "/.docker"
	)
	ops := []string{"file-read*", "file-write*", "file-ioctl", "file-map-executable", "process-exec*", "network-outbound"}

	for _, mode := range []string{sandboxprofile.ModeFiltered, sandboxprofile.ModeBlocked, sandboxprofile.ModeOpen} {
		for _, enforcement := range []string{sandboxprofile.EnforceKernel, sandboxprofile.EnforceEnvOnly} {
			t.Run(mode+"/"+enforcement, func(t *testing.T) {
				g := &Grants{
					Workdir:         home + "/work",
					AllowPaths:      []string{home, protected + "/run/docker.sock"},
					ProtectedPaths:  []string{protected},
					NameDenyRoots:   []string{home},
					NameDenyGlobs:   []string{".env"},
					NetworkMode:     mode,
					Enforcement:     enforcement,
					ProxyPort:       54321,
					ListenPorts:     []int{4097},
					AllowTCPConnect: []int{22},
					OpenPorts:       []int{0},
					UnixSockets:     []string{protected + "/run/docker.sock"},
					UnixSocketDirs:  []string{protected + "/run"},
				}
				lines := strings.Split(GenerateSBPL(g), "\n")

				lastAllow := -1
				for i, l := range lines {
					if strings.HasPrefix(l, "(allow ") {
						lastAllow = i
					}
				}
				var denies []string
				for _, op := range ops {
					denies = append(denies, fmt.Sprintf("(deny %s (subpath %q))", op, protected))
				}
				res, err := sbplNameRegex(home, g.NameDenyGlobs)
				if err != nil {
					t.Fatal(err)
				}
				for _, op := range ops {
					denies = append(denies, fmt.Sprintf("(deny %s (regex #\"%s\"))", op, res[0]))
				}
				for _, deny := range denies {
					i := indexOfLine(lines, deny)
					if i < 0 {
						t.Errorf("profile lacks %s", deny)
						continue
					}
					if i < lastAllow {
						t.Errorf("%s comes before %s; Seatbelt is last-match-wins, so that allow can reopen the denied path", deny, lines[lastAllow])
					}
				}
			})
		}
	}
}

func indexOfLine(lines []string, want string) int {
	for i, l := range lines {
		if l == want {
			return i
		}
	}
	return -1
}
