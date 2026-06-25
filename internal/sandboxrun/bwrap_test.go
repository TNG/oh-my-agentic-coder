package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
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
	// --new-session detaches the controlling terminal and breaks SIGWINCH
	// resize propagation into the inner TUI; it must stay out.
	if strings.Contains(joined, "--new-session") {
		t.Error("must NOT use --new-session (breaks terminal resize/SIGWINCH)")
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
