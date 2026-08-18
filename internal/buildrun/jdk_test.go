package buildrun

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// makeFakeJDK creates a JDK-shaped tree at <root>/bin/java (an executable
// regular file), <root>/lib/ (a dir), and <root>/conf/security/java.security
// (the JDK 9+ layout the JVM's Security.initialize() reads; the JDK 8
// flat layout keeps it at lib/security/java.security). Returning the JDK
// home (root). The java binary is a STUB with a Mach-O magic header
// (0xfeedface) so realJava's isShellScript check does not reject it as a
// `#!` shim — a real java starts with ELF/Mach-O magic, never `#!`. Only
// its existence + exec bit + non-shebang header matter for resolution.
func makeFakeJDK(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	// JDK 9+ layout: conf/security/java.security is the security config
	// file the JVM reads at startup. Without a read grant on conf/, a
	// sandboxed JVM dies with java.lang.InternalError "Error loading
	// java.security file" (the canary's CI failure).
	if err := os.MkdirAll(filepath.Join(root, "conf", "security"), 0o755); err != nil {
		t.Fatal(err)
	}
	javaSecurity := []byte("security.provider.1=com.example.Provider\n")
	if err := os.WriteFile(filepath.Join(root, "conf", "security", "java.security"), javaSecurity, 0o644); err != nil {
		t.Fatal(err)
	}
	java := filepath.Join(bin, "java")
	// Mach-O magic (0xFE 0xED 0xFA 0xCE) — a non-`#!` header so the
	// shim-script rejection does not fire; the file is never exec'd.
	if err := os.WriteFile(java, []byte{0xFE, 0xED, 0xFA, 0xCE}, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// envMap builds a getenv closure from a map.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveJDK_RealJavaHome(t *testing.T) {
	jdk := makeFakeJDK(t, filepath.Join(t.TempDir(), "jdk"))
	r, err := ResolveJDK(envMap(map[string]string{
		"JAVA_HOME": jdk,
		"PATH":      "/usr/bin:/bin",
	}))
	if err != nil {
		t.Fatalf("ResolveJDK: %v", err)
	}
	if r.JavaHome != jdk {
		t.Errorf("JavaHome = %q, want %q", r.JavaHome, jdk)
	}
	wantBin := filepath.Join(jdk, "bin")
	if r.BinDir != wantBin {
		t.Errorf("BinDir = %q, want %q", r.BinDir, wantBin)
	}
	// Real JDK bin must be prepended to PATH.
	if !strings.HasPrefix(r.Path, wantBin+string(filepath.ListSeparator)) {
		t.Errorf("PATH = %q, want %q prepended", r.Path, wantBin)
	}
	// ReadPaths must include the bin dir so Seatbelt allows exec.
	if !contains(r.ReadPaths, wantBin) {
		t.Errorf("ReadPaths missing bin %s: %v", wantBin, r.ReadPaths)
	}
}

func TestResolveJDK_JenvShimOnPath(t *testing.T) {
	tmp := t.TempDir()
	realJDK := makeFakeJDK(t, filepath.Join(tmp, "real-jdk"))
	// Build a jenv-shim tree: <tmp>/.jenv/versions/<v>/bin/java -> real java.
	shimHome := filepath.Join(tmp, ".jenv", "versions", "17.0")
	shimBin := filepath.Join(shimHome, "bin")
	if err := os.MkdirAll(shimBin, 0o755); err != nil {
		t.Fatal(err)
	}
	realJava := filepath.Join(realJDK, "bin", "java")
	if err := os.Symlink(realJava, filepath.Join(shimBin, "java")); err != nil {
		t.Fatal(err)
	}
	// Also a literal jenv shims dir pointing at the same shim (jenv puts
	// ~/.jenv/shims on PATH). Each shim is itself a symlink to the version.
	shimsDir := filepath.Join(tmp, ".jenv", "shims")
	if err := os.MkdirAll(shimsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realJava, filepath.Join(shimsDir, "java")); err != nil {
		t.Fatal(err)
	}
	path := shimsDir + string(filepath.ListSeparator) + "/usr/bin" + string(filepath.ListSeparator) + "/bin"
	r, err := ResolveJDK(envMap(map[string]string{
		"JAVA_HOME": "", // unset: must discover via PATH
		"PATH":      path,
	}))
	if err != nil {
		t.Fatalf("ResolveJDK: %v", err)
	}
	if r.JavaHome != realJDK {
		t.Errorf("JavaHome = %q, want real JDK %q (shim must be bypassed)", r.JavaHome, realJDK)
	}
	// Both jenv shim entries must be stripped from PATH.
	for _, p := range filepath.SplitList(r.Path) {
		if strings.Contains(p, ".jenv") {
			t.Errorf("PATH still contains jenv entry %q: %q", p, r.Path)
		}
	}
	// Real JDK bin must be prepended.
	wantBin := filepath.Join(realJDK, "bin")
	if !strings.HasPrefix(r.Path, wantBin+string(filepath.ListSeparator)) {
		t.Errorf("PATH = %q, want %q prepended after shim stripping", r.Path, wantBin)
	}
	// ReadPaths must grant the real bin (not the shim).
	if !contains(r.ReadPaths, wantBin) {
		t.Errorf("ReadPaths missing real bin %s: %v", wantBin, r.ReadPaths)
	}
	for _, p := range r.ReadPaths {
		if strings.Contains(p, ".jenv") {
			t.Errorf("ReadPaths grants a jenv path %q — must be the real JDK only", p)
		}
	}
}

func TestResolveJDK_NoJavaAnywhere(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveJDK(envMap(map[string]string{
		"JAVA_HOME": "",
		"PATH":      empty, // no java here
	}))
	if err == nil {
		t.Fatal("expected error when no JDK is discoverable")
	}
}

// TestResolveJDK_RejectsShimScriptOnPath: a jenv/asdf shim that is a
// shell script (#!/bin/sh\nexec ...) on PATH — NOT a symlink — must be
// rejected by realJava (the root cause of ticket-04 host failure where
// JAVA_HOME resolved to the jenv ROOT ~/.jenv). A real java binary (a
// non-`#!` stub with Mach-O magic) on a later PATH entry is accepted.
func TestResolveJDK_RejectsShimScriptOnPath(t *testing.T) {
	tmp := t.TempDir()
	// jenv shims dir: java is a shell SCRIPT (the real jenv layout).
	shimsDir := filepath.Join(tmp, "shims")
	if err := os.MkdirAll(shimsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shimJava := filepath.Join(shimsDir, "java")
	if err := os.WriteFile(shimJava, []byte("#!/bin/sh\nexec \"$JENV_DIR/shims/java\" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A real JDK later on PATH (must win after the shim is rejected).
	realJDK := makeFakeJDK(t, filepath.Join(tmp, "real-jdk"))
	realBin := filepath.Join(realJDK, "bin")

	path := shimsDir + string(filepath.ListSeparator) + realBin + string(filepath.ListSeparator) + "/usr/bin"
	r, err := ResolveJDK(envMap(map[string]string{
		"JAVA_HOME": "",
		"PATH":      path,
	}))
	if err != nil {
		t.Fatalf("ResolveJDK: %v", err)
	}
	if r.JavaHome != realJDK {
		t.Errorf("JavaHome = %q, want real JDK %q (shim script must be rejected)", r.JavaHome, realJDK)
	}
}

// TestResolveJDK_JenvRootAsJavaHomeRejected: JAVA_HOME pointing at the
// jenv ROOT (~/.jenv, which has no bin/java) must NOT resolve; the
// resolver falls back to PATH. This is the exact ticket-04 host failure
// (JAVA_HOME=/Users/.../.jenv).
func TestResolveJDK_JenvRootAsJavaHomeRejected(t *testing.T) {
	tmp := t.TempDir()
	// jenv root dir: has a `bin` (jenv's own bin, NOT a JDK bin) but no
	// bin/java. Real jenv ~/.jenv/bin holds the `jenv` tool, not java.
	jenvRoot := filepath.Join(tmp, ".jenv")
	if err := os.MkdirAll(filepath.Join(jenvRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	realJDK := makeFakeJDK(t, filepath.Join(tmp, "real-jdk"))
	path := filepath.Join(realJDK, "bin") + string(filepath.ListSeparator) + "/usr/bin"
	r, err := ResolveJDK(envMap(map[string]string{
		"JAVA_HOME": jenvRoot, // bogus: jenv root, not a JDK
		"PATH":      path,
	}))
	if err != nil {
		t.Fatalf("ResolveJDK: %v", err)
	}
	if r.JavaHome != realJDK {
		t.Errorf("JavaHome = %q, want fallback to PATH JDK %q (jenv root must not resolve)", r.JavaHome, realJDK)
	}
}

// TestRealJava_RejectsShimScriptAcceptsStubBinary is the unit proof for
// the isShellScript gate: a `#!`-prefixed regular executable is rejected,
// a Mach-O-magic stub regular executable is accepted.
func TestRealJava_RejectsShimScriptAcceptsStubBinary(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "shim")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec java \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := realJava(shim); ok {
		t.Error("realJava accepted a #! shim script — must reject")
	}
	stub := filepath.Join(dir, "real")
	if err := os.WriteFile(stub, []byte{0xFE, 0xED, 0xFA, 0xCE}, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, ok := realJava(stub); !ok {
		t.Error("realJava rejected a non-#! stub binary — must accept")
	} else if got != stub {
		t.Errorf("realJava = %q, want %q", got, stub)
	}
}

func TestResolveJDK_BadJavaHomeFallsBackToPath(t *testing.T) {
	tmp := t.TempDir()
	realJDK := makeFakeJDK(t, filepath.Join(tmp, "real"))
	// JAVA_HOME points at a nonexistent dir — resolver must fall back to
	// discovering java on PATH rather than trusting a broken JAVA_HOME.
	bogus := filepath.Join(tmp, "bogus-java-home")
	path := filepath.Join(realJDK, "bin") + string(filepath.ListSeparator) + "/usr/bin"
	r, err := ResolveJDK(envMap(map[string]string{
		"JAVA_HOME": bogus,
		"PATH":      path,
	}))
	if err != nil {
		t.Fatalf("ResolveJDK: %v", err)
	}
	if r.JavaHome != realJDK {
		t.Errorf("JavaHome = %q, want fallback to PATH-discovered %q", r.JavaHome, realJDK)
	}
}

func TestResolveJDK_ReadPathsIncludeLib(t *testing.T) {
	jdk := makeFakeJDK(t, filepath.Join(t.TempDir(), "jdk"))
	r, err := ResolveJDK(envMap(map[string]string{
		"JAVA_HOME": jdk,
		"PATH":      "/usr/bin",
	}))
	if err != nil {
		t.Fatalf("ResolveJDK: %v", err)
	}
	if !contains(r.ReadPaths, filepath.Join(jdk, "lib")) {
		t.Errorf("ReadPaths missing JDK lib dir: %v", r.ReadPaths)
	}
	if !contains(r.ReadPaths, filepath.Join(jdk, "conf")) {
		t.Errorf("ReadPaths missing JDK conf dir (java.security): %v", r.ReadPaths)
	}
}

func TestResolveJDK_DeterministicReadPaths(t *testing.T) {
	jdk := makeFakeJDK(t, filepath.Join(t.TempDir(), "jdk"))
	r1, _ := ResolveJDK(envMap(map[string]string{"JAVA_HOME": jdk, "PATH": "/usr/bin"}))
	r2, _ := ResolveJDK(envMap(map[string]string{"JAVA_HOME": jdk, "PATH": "/usr/bin"}))
	if runtime.GOOS != "darwin" {
		// Path sorting only matters for determinism across the platform
		// backends; assert order-independent equality via sorted compare.
		s := func(p []string) []string {
			cp := append([]string{}, p...)
			sort.Strings(cp)
			return cp
		}
		if !equalStrings(s(r1.ReadPaths), s(r2.ReadPaths)) {
			t.Errorf("non-deterministic ReadPaths: %v vs %v", r1.ReadPaths, r2.ReadPaths)
		}
	}
}

func contains(list []string, want string) bool {
	for _, p := range list {
		if p == want {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
