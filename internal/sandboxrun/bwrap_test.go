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

// issue #302: a grant whose destination is a symlink inside an
// already-mounted tree kills bwrap ("Can't mount on symlink
// destination"), because the destination exists in the sandbox as that
// symlink. The bind must be emitted at the symlink-resolved path instead.
func TestBwrapSymlinkMountUnderBoundAncestorRedirected(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	lnk := filepath.Join(root, "lnk")
	if err := os.Symlink(real, lnk); err != nil {
		t.Fatal(err)
	}
	// rw grant via the symlink spelling, ro grant of the resolved path:
	// the rewrite must merge them with rw winning.
	g := &Grants{
		Workdir:     root,
		ReadPaths:   []string{root, real},
		WritePaths:  []string{lnk},
		NetworkMode: sandboxprofile.ModeBlocked,
	}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "--bind "+lnk+" "+lnk) {
		t.Errorf("symlink spelling must not be a bind destination (bwrap refuses to mount on it): %s", joined)
	}
	if strings.Count(joined, real+" "+real) != 1 {
		t.Errorf("resolved path must be bound exactly once: %s", joined)
	}
	if !strings.Contains(joined, "--bind "+real+" "+real) {
		t.Errorf("rw grant must survive the rewrite as --bind: %s", joined)
	}
}

// A symlink grant with no mounted ancestor is left at its own spelling:
// bwrap creates the destination as a fresh directory holding the target's
// content, and the spelling itself must stay usable (an ELF interpreter
// path like /lib64/ld-linux-x86-64.so.2 depends on the spelling, not the
// resolved path, when /usr is not yet mounted).
func TestBwrapSymlinkMountWithoutAncestorKept(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	lnk := filepath.Join(dir, "lnk")
	if err := os.Symlink(target, lnk); err != nil {
		t.Fatal(err)
	}
	g := &Grants{
		Workdir:     dir,
		ReadPaths:   []string{lnk}, // dir itself NOT granted
		NetworkMode: sandboxprofile.ModeBlocked,
	}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--ro-bind "+lnk+" "+lnk) {
		t.Errorf("unshadowed symlink grant must keep its own spelling: %s", joined)
	}
	if strings.Contains(joined, target+" "+target) {
		t.Errorf("no rewrite without a covering ancestor: %s", joined)
	}
}

// With the whole root granted (learn mode), the emit loop drops every
// other bind (--bind / / already covers them, rw), so no symlink
// destination is ever emitted. The rewrite pass still normalizes the
// mount map: the symlink spelling must not appear as a bind.
func TestBwrapSymlinkMountDroppedUnderRootGrant(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	lnk := filepath.Join(dir, "lnk")
	if err := os.Symlink(target, lnk); err != nil {
		t.Fatal(err)
	}
	g := &Grants{
		Workdir:     dir,
		AllowPaths:  []string{"/", lnk},
		NetworkMode: sandboxprofile.ModeBlocked,
	}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--bind / /") {
		t.Errorf("root grant must bind the whole tree rw: %s", joined)
	}
	if strings.Contains(joined, lnk) {
		t.Errorf("under a root grant the symlink spelling must not be emitted as a bind: %s", joined)
	}
}

// A protected path that is itself a symlink cannot be masked at its own
// spelling (same bwrap refusal). The mask must land on the resolved
// target, so reads through the spelling hit the marker.
func TestBwrapProtectedSymlinkMaskedAtResolvedTarget(t *testing.T) {
	home := t.TempDir()
	secretDir := filepath.Join(home, "secrets")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(secretDir, "env")
	if err := os.WriteFile(secretFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	wd := filepath.Join(home, "proj")
	if err := os.Mkdir(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	linkEnv := filepath.Join(wd, ".env")
	if err := os.Symlink(secretFile, linkEnv); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(wd, "ssh")
	if err := os.Symlink(secretDir, linkDir); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(wd, "broken")
	if err := os.Symlink(filepath.Join(home, "does-not-exist"), broken); err != nil {
		t.Fatal(err)
	}
	g := &Grants{
		Workdir:        wd,
		AllowPaths:     []string{wd},
		ProtectedPaths: []string{linkEnv, linkDir, broken},
		NetworkMode:    sandboxprofile.ModeBlocked,
	}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--ro-bind /dev/null "+secretFile) {
		t.Errorf("symlinked protected file must be masked at its resolved target: %s", joined)
	}
	if !strings.Contains(joined, "--tmpfs "+secretDir) {
		t.Errorf("symlinked protected dir must be masked at its resolved target: %s", joined)
	}
	if strings.Contains(joined, "--ro-bind /dev/null "+linkEnv) || strings.Contains(joined, "--tmpfs "+linkDir) {
		t.Errorf("masks must not land on the symlink spellings (bwrap refuses to mount on them): %s", joined)
	}
	if strings.Contains(joined, broken) {
		t.Errorf("a broken protected symlink reads as ENOENT and must be skipped, not masked: %s", joined)
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
