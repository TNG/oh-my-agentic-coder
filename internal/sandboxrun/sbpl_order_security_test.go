package sandboxrun

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestSecurityNoAllowAfterProtectedDenies guards the rule that a protected
// path is closed to every operation that can name a path and that no later
// rule can reopen it. Seatbelt is last-match-wins, so any allow emitted after
// a deny may override it.
func TestSecurityNoAllowAfterProtectedDenies(t *testing.T) {
	const (
		home      = "/Users/u"
		protected = home + "/.docker"
	)
	const rootStore = `(allow file-read* (subpath "/System/Library/Keychains"))`

	for _, mode := range []string{sandboxprofile.ModeFiltered, sandboxprofile.ModeBlocked, sandboxprofile.ModeOpen} {
		for _, enforcement := range []string{sandboxprofile.EnforceKernel, sandboxprofile.EnforceEnvOnly} {
			t.Run(mode+"/"+enforcement, func(t *testing.T) {
				g := securityTestGrants(home, protected, mode, enforcement)
				lines := strings.Split(GenerateSBPL(g), "\n")

				// The root-store re-allow is the one allow that may follow a
				// deny, and only the keychain deny.
				lastAllow, lastOtherAllow := -1, -1
				for i, l := range lines {
					if strings.HasPrefix(l, "(allow ") {
						lastAllow = i
						if l != rootStore {
							lastOtherAllow = i
						}
					}
				}
				res, err := sbplNameRegex(home, g.NameDenyGlobs)
				if err != nil {
					t.Fatal(err)
				}
				if len(res) != len(g.NameDenyGlobs) {
					t.Fatalf("want one regex per glob %v, got %v", g.NameDenyGlobs, res)
				}
				filters := map[string]int{
					fmt.Sprintf("(subpath %q)", protected):                lastAllow,
					fmt.Sprintf("(regex #\"%s\")", sbplKeychainNameRegex): lastOtherAllow,
				}
				for _, re := range res {
					filters[fmt.Sprintf("(regex #\"%s\")", re)] = lastAllow
				}
				for filter, last := range filters {
					for _, op := range sbplDeniedOps {
						deny := fmt.Sprintf("(deny %s %s)", op, filter)
						i := indexOfLine(lines, deny)
						if i < 0 {
							t.Errorf("profile lacks %s", deny)
							continue
						}
						if i < last {
							t.Errorf("%s comes before %s; Seatbelt is last-match-wins, so that allow can reopen the denied path", deny, lines[last])
						}
					}
				}
			})
		}
	}
}

// TestSecurityDeniedOpsCoverEveryAllowedOp fails when the profile starts to
// allow an operation that protected paths do not deny, so a new allow cannot
// quietly reopen them.
func TestSecurityDeniedOpsCoverEveryAllowedOp(t *testing.T) {
	// Operations that take no path filter, so a path deny cannot apply.
	pathless := []string{"process-fork", "process-info*", "signal", "mach-lookup", "sysctl-read", "ipc-posix-shm", "system-socket", "pseudo-tty"}
	// Binding a Unix socket creates a file, which file-write* denies
	// (TestIntegrationProtectedFolderRefusesSocketConnect checks this on
	// macOS); a connect is network-outbound. TCP has no path.
	coveredBy := map[string]string{
		"network-bind":    "file-write*",
		"network-inbound": "file-write*",
		"network*":        "network-outbound",
	}
	denied := func(op string) bool {
		return slices.ContainsFunc(sbplDeniedOps, func(d string) bool {
			return d == op || strings.HasSuffix(d, "*") && strings.HasPrefix(op, strings.TrimSuffix(d, "*"))
		})
	}
	for _, mode := range []string{sandboxprofile.ModeFiltered, sandboxprofile.ModeBlocked, sandboxprofile.ModeOpen} {
		for _, enforcement := range []string{sandboxprofile.EnforceKernel, sandboxprofile.EnforceEnvOnly} {
			g := securityTestGrants("/Users/u", "/Users/u/.docker", mode, enforcement)
			for _, l := range strings.Split(GenerateSBPL(g), "\n") {
				rest, ok := strings.CutPrefix(l, "(allow ")
				if !ok {
					continue
				}
				for _, op := range strings.Fields(strings.SplitN(strings.TrimSuffix(rest, ")"), "(", 2)[0]) {
					if slices.Contains(pathless, op) {
						continue
					}
					want := op
					if c, ok := coveredBy[op]; ok {
						want = c
					}
					if !denied(want) {
						t.Errorf("%s/%s: %q allows %s, which protected paths do not deny; add it to sbplDeniedOps", mode, enforcement, l, op)
					}
				}
			}
		}
	}
}

func securityTestGrants(home, protected, mode, enforcement string) *Grants {
	return &Grants{
		Workdir:         home + "/work",
		AllowPaths:      []string{home, protected + "/run/docker.sock"},
		ProtectedPaths:  []string{protected},
		NameDenyRoots:   []string{home},
		NameDenyGlobs:   []string{".env", "*.pem"},
		NetworkMode:     mode,
		Enforcement:     enforcement,
		ProxyPort:       54321,
		ListenPorts:     []int{4097},
		AllowTCPConnect: []int{22},
		OpenPorts:       []int{0},
		UnixSockets:     []string{protected + "/run/docker.sock"},
		UnixSocketDirs:  []string{protected + "/run"},
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
