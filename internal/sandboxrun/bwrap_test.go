package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func bwrapGrants() *Grants {
	return &Grants{
		Workdir:         "/work",
		ReadPaths:       []string{"/usr", "/etc", "/cfg"},
		WritePaths:      []string{"/scratch"},
		AllowPaths:      []string{"/work"},
		ProtectedPaths:  []string{"/home/u/.ssh"},
		NetworkMode:     sandboxprofile.ModeFiltered,
		Enforcement:     sandboxprofile.EnforceKernel,
		ProxyPort:       54321,
		ListenPorts:     []int{4097},
		AllowTCPConnect: []int{22},
		OpenPorts:       []int{49152},
	}
}

func TestBwrapArgvStructure(t *testing.T) {
	argv, err := BuildBwrapArgv(bwrapGrants(), []string{"/omac", "sandbox", "stage2", "--", "inner"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"bwrap",
		"--die-with-parent",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--proc /proc",
		"--dev /dev",
		"--ro-bind /usr /usr",
		"--ro-bind /etc /etc",
		"--ro-bind /cfg /cfg",
		"--bind /scratch /scratch",
		"--bind /work /work",
		"--tmpfs /tmp",
		"--chdir /work",
		"-- /omac sandbox stage2 -- inner",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q in: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--unshare-net") {
		t.Error("network namespace must NOT be unshared")
	}
	if !strings.Contains(joined, "--new-session") {
		t.Error("must use --new-session to prevent TIOCSTI keystroke injection")
	}
}

func TestBwrapNoTmpfsWhenTmpGranted(t *testing.T) {
	g := bwrapGrants()
	g.WritePaths = append(g.WritePaths, "/tmp")
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "--tmpfs /tmp") {
		t.Error("granted /tmp must not be shadowed by tmpfs")
	}
	if !strings.Contains(joined, "--bind /tmp /tmp") {
		t.Error("granted /tmp must be bound rw")
	}
}

func TestBwrapAllowWinsOverRead(t *testing.T) {
	g := bwrapGrants()
	g.ReadPaths = append(g.ReadPaths, "/work") // same path read+allow
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "--ro-bind /work /work") {
		t.Error("rw grant must win over ro for the same path")
	}
	if strings.Count(joined, " /work /work") != 1 {
		t.Errorf("duplicate binds for /work: %s", joined)
	}
}

func TestBwrapProtectedPathMasking(t *testing.T) {
	// Protected dir inside a granted tree -> tmpfs mask. Use real
	// paths so Lstat works.
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	netrc := filepath.Join(home, ".netrc")
	if err := os.WriteFile(netrc, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &Grants{
		Workdir:        home,
		AllowPaths:     []string{home},
		ProtectedPaths: []string{sshDir, netrc, filepath.Join(home, ".nonexistent")},
		NetworkMode:    sandboxprofile.ModeBlocked,
	}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--tmpfs "+sshDir) {
		t.Errorf("protected dir not masked: %s", joined)
	}
	if !strings.Contains(joined, "--ro-bind /dev/null "+netrc) {
		t.Errorf("protected file not masked: %s", joined)
	}
	if strings.Contains(joined, ".nonexistent") {
		t.Error("nonexistent protected path must not be masked")
	}
	// Mask must come after the bind it shadows.
	bindPos := strings.Index(joined, "--bind "+home)
	maskPos := strings.Index(joined, "--tmpfs "+sshDir)
	if bindPos < 0 || maskPos < bindPos {
		t.Error("mask must come after the covering bind")
	}
}

func TestBwrapProtectedOutsideGrantsNotMasked(t *testing.T) {
	g := &Grants{
		Workdir:        "/work",
		AllowPaths:     []string{"/work"},
		ProtectedPaths: []string{"/home/u/.ssh"}, // not covered by any bind
		NetworkMode:    sandboxprofile.ModeBlocked,
	}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argv, " "), ".ssh") {
		t.Error("uncovered protected path needs no mask (it is absent)")
	}
}

func TestBwrapUnixSocketDirNonexistentUsesBindTry(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "cc-daemon-502")
	g := bwrapGrants()
	g.UnixSocketDirs = []string{missing}
	g.AllowPaths = append(g.AllowPaths, missing) // mirrors ResolveGrants
	argv, err := BuildBwrapArgv(g, []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "--bind "+missing+" "+missing) {
		t.Errorf("missing unix dir must not use --bind (aborts launch): %s", joined)
	}
	if !strings.Contains(joined, "--bind-try "+missing+" "+missing) {
		t.Errorf("missing unix dir must use --bind-try: %s", joined)
	}
}

func TestBwrapUnixSocketDirExistingIsBound(t *testing.T) {
	dir := t.TempDir()
	g := bwrapGrants()
	g.UnixSocketDirs = []string{dir}
	g.AllowPaths = append(g.AllowPaths, dir)
	argv, err := BuildBwrapArgv(g, []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--bind-try "+dir+" "+dir) {
		t.Errorf("existing unix dir must be bound rw: %s", joined)
	}
}

func TestBwrapUnixSocketDirMissingUnderTmpKeepsTmpfs(t *testing.T) {
	g := bwrapGrants()
	g.UnixSocketDirs = []string{"/tmp/cc-daemon-does-not-exist-9999"}
	g.AllowPaths = append(g.AllowPaths, "/tmp/cc-daemon-does-not-exist-9999")
	argv, err := BuildBwrapArgv(g, []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(argv, " "), "--tmpfs /tmp") {
		t.Errorf("missing /tmp unix dir must not suppress tmpfs /tmp: %v", argv)
	}
}

// symlinkAt creates link -> target, making link's parent dir first.
func symlinkAt(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// Issue #302: inside the sandbox, a granted symlink that sits in another
// grant already exists as that symlink, and bwrap >= 0.12 refuses to mount
// onto it. When the link's target is visible anyway, the bind is dropped.
func TestBwrapNestedSymlinkGrantDropped(t *testing.T) {
	usr := filepath.Join(t.TempDir(), "usr")
	if err := os.MkdirAll(filepath.Join(usr, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkAt(t, "lib", filepath.Join(usr, "lib64")) // Arch: /usr/lib64 -> lib
	g := &Grants{Workdir: "/", ReadPaths: []string{usr, filepath.Join(usr, "lib64")}}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "lib64") {
		t.Errorf("nested symlink must not be a bind destination: %s", joined)
	}
	if !strings.Contains(joined, "--ro-bind "+usr+" "+usr) {
		t.Errorf("parent grant missing: %s", joined)
	}
}

// A symlink in a granted tree may have been planted by the agent, so its
// target must never become a bind of its own. Where dropping the bind would
// change access, it stays as is and bwrap refuses to start.
func TestBwrapNestedSymlinkGrantKeptUnlessRedundant(t *testing.T) {
	wd := t.TempDir()
	data := filepath.Join(wd, "data")
	if err := os.Mkdir(data, 0o755); err != nil {
		t.Fatal(err)
	}
	toRoot, toData := filepath.Join(wd, "lib"), filepath.Join(wd, "data-link")
	symlinkAt(t, "/", toRoot)
	symlinkAt(t, "data", toData)

	for _, tc := range []struct {
		name string
		link string
		g    *Grants
	}{
		{"target outside every grant", toRoot, &Grants{Workdir: wd, AllowPaths: []string{wd}, ReadPaths: []string{toRoot}}},
		{"rw link into a ro tree", toData, &Grants{Workdir: wd, ReadPaths: []string{wd}, AllowPaths: []string{toData}}},
		{"ro link into a rw tree", toData, &Grants{Workdir: wd, AllowPaths: []string{wd}, ReadPaths: []string{toData}}},
	} {
		argv, err := BuildBwrapArgv(tc.g, []string{"x"})
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, " / / ") || strings.Contains(joined, " "+data+" "+data) {
			t.Errorf("%s: symlink target bound: %s", tc.name, joined)
		}
		if !strings.Contains(joined, " "+tc.link+" "+tc.link) {
			t.Errorf("%s: grant must stay for bwrap to refuse: %s", tc.name, joined)
		}
	}
}

// A protected symlink keeps its mask in place, so bwrap >= 0.12 refuses to
// start. Moving the mask to the link's target is unsafe: the same secret can
// stay visible under another name, e.g. when the link is itself a grant
// (bwrap then binds the target's content at the link's name) or the target
// is reachable through a symlinked grant.
func TestBwrapProtectedSymlinkMaskedInPlace(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "netrc"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	netrc := filepath.Join(home, ".netrc")
	symlinkAt(t, "netrc", netrc)
	g := &Grants{Workdir: "/", ReadPaths: []string{home, netrc}, ProtectedPaths: []string{netrc}}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--ro-bind /dev/null "+netrc) {
		t.Errorf("protected symlink must be masked in place: %s", joined)
	}
	if strings.Contains(joined, "/dev/null "+filepath.Join(home, "netrc")) {
		t.Errorf("mask must not move to the link's target: %s", joined)
	}
}

// symlinkMountDests finds the mounts bwrap >= 0.12 refuses: those onto a
// symlink inside an earlier bind, whether a protected path's mask or a
// granted symlink kept by BuildBwrapArgv.
func TestSymlinkMountDests(t *testing.T) {
	home, other := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, jdk, solo := filepath.Join(home, ".env"), filepath.Join(home, "jdk"), filepath.Join(other, "solo")
	symlinkAt(t, "/nonexistent/.env", env)
	symlinkAt(t, "/opt/jdk", jdk)
	symlinkAt(t, home, solo) // not inside another bind: bwrap creates a fresh dir
	g := &Grants{
		Workdir:        home,
		AllowPaths:     []string{home},
		ReadPaths:      []string{jdk, solo},
		ProtectedPaths: []string{env, filepath.Join(home, ".netrc")},
	}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := symlinkMountDests(argv), []string{jdk, env}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStage2ArgsFiltered(t *testing.T) {
	got := Stage2Args(bwrapGrants())
	want := []string{
		"--connect-tcp", "22",
		"--connect-tcp", "49152",
		"--connect-tcp", "54321",
		"--bind-tcp", "4097",
		"--bind-tcp", "49152",
		"--enforce",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Stage2Args = %v, want %v", got, want)
	}
}

func TestStage2ArgsBlocked(t *testing.T) {
	g := bwrapGrants()
	g.NetworkMode = sandboxprofile.ModeBlocked
	got := Stage2Args(g)
	if !slices.Equal(got, []string{"--enforce"}) {
		t.Errorf("blocked mode = %v, want bare --enforce (full TCP block)", got)
	}
}

func TestStage2ArgsOpenAndEnvOnly(t *testing.T) {
	g := bwrapGrants()
	g.NetworkMode = sandboxprofile.ModeOpen
	if got := Stage2Args(g); len(got) != 0 {
		t.Errorf("open mode = %v, want none", got)
	}
	g2 := bwrapGrants()
	g2.Enforcement = sandboxprofile.EnforceEnvOnly
	if got := Stage2Args(g2); len(got) != 0 {
		t.Errorf("env-only = %v, want none", got)
	}
}
