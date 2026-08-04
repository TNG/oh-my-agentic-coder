package buildrun

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// JDKResolution is the resolved real JDK the executor will use. Shims
// (jenv, asdf, SDKMAN) are bypassed: the executor under a deny-default
// kernel sandbox cannot run a shim that depends on process substitution
// (/dev/fd) or shell functions, so the real JDK bin/lib are granted and
// JAVA_HOME/PATH are rewritten to point at them.
type JDKResolution struct {
	// JavaHome is the real JDK install root (the parent of its bin/ dir).
	// Empty only when no JDK could be discovered.
	JavaHome string
	// BinDir is JavaHome/bin (the dir holding java/javac).
	BinDir string
	// Path is the corrected PATH for the child env: the real JDK bin
	// prepended, any shim dirs stripped, the rest of the parent PATH kept.
	Path string
	// ReadPaths are the dirs Seatbelt must grant read/exec access to so the
	// JDK can exec and load its native libs: BinDir plus the JDK's install-
	// prefix support dirs (lib/libexec/...).
	ReadPaths []string
}

// getenv is the seam tests use to inject a parent environment without
// touching the real process env. Production passes os.Getenv.
type getenv func(string) string

// ResolveJDK discovers the real JDK the executor will run under, bypassing
// version-manager shims (jenv/asdf/SDKMAN). Resolution order:
//
//  1. If JAVA_HOME points at a real JDK (its bin/java exists as an
//     executable regular file or a symlink chain to one), use it.
//  2. Otherwise, walk PATH left-to-right; the first java entry that
//     resolves (via os.Readlink chains and EvalSymlinks) to a real JDK
//     install wins.
//
// A "real JDK" is one whose java binary is a regular executable file (or a
// chain of symlinks ending in one) — NOT a shim that requires shell
// process substitution or a function wrapper. jenv/asdf shims are
// typically symlinks pointing at the real version's java, so following the
// chain is enough; a shim that is itself a shell script is rejected.
//
// /usr/libexec/java_home is deliberately NOT used: the loopback spike
// (REPORT.md:112) found it pointing at a nonexistent JDK on a real host.
// It would only be a fallback, and a wrong fallback is worse than none.
func ResolveJDK(env getenv) (JDKResolution, error) {
	parentPath := env("PATH")
	javaHome := env("JAVA_HOME")

	// 1. Try JAVA_HOME first if it points at a real JDK.
	if jdk, ok := jdkFromHome(javaHome); ok {
		return buildJDKResolution(jdk, parentPath), nil
	}

	// 2. Walk PATH for a real java.
	if jdk, ok := jdkFromPath(parentPath); ok {
		return buildJDKResolution(jdk, parentPath), nil
	}

	return JDKResolution{}, errors.New("no real JDK found: the java on PATH is a jenv/asdf/SDKMAN shim script (rejected); set JAVA_HOME to a real JDK install root (e.g. ~/.jenv/versions/<v>) and ensure its bin/java is a native binary, not a shim")
}

// jdkFromHome returns the real JDK root if home/bin/java is (a chain of
// symlinks to) an executable regular file. An empty or nonexistent home
// yields ok=false.
func jdkFromHome(home string) (string, bool) {
	if home == "" {
		return "", false
	}
	java := filepath.Join(home, "bin", "java")
	if resolved, ok := realJava(java); ok {
		// The JDK root is the parent of the bin dir of the resolved java.
		binDir := filepath.Dir(resolved)
		return filepath.Dir(binDir), true
	}
	return "", false
}

// jdkFromPath walks PATH entries left-to-right; the first real java found
// wins. A PATH entry that is itself a jenv shims dir contributes its java
// only via the symlink chain (handled by realJava).
func jdkFromPath(path string) (string, bool) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		java := filepath.Join(dir, "java")
		if resolved, ok := realJava(java); ok {
			binDir := filepath.Dir(resolved)
			return filepath.Dir(binDir), true
		}
	}
	return "", false
}

// realJava resolves java through a chain of symlinks (os.Readlink +
// EvalSymlinks) and reports the final target as a real JDK java binary:
// an executable regular file. A shim that is a shell script (not a
// symlink, or a symlink to a script) is rejected — shims need process
// substitution that the kernel sandbox denies.
func realJava(java string) (string, bool) {
	// Lstat first: a missing entry is not a real java.
	fi, err := os.Lstat(java)
	if err != nil {
		return "", false
	}
	// Follow the symlink chain manually first so a chain that loops or
	// escapes is bounded; EvalSymlinks would also do this but Readlink
	// lets us reject a chain that ends in a non-regular file explicitly.
	cur := java
	visited := map[string]bool{}
	for fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(cur)
		if err != nil {
			return "", false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		cur = filepath.Clean(target)
		if visited[cur] {
			return "", false // symlink loop
		}
		visited[cur] = true
		fi, err = os.Lstat(cur)
		if err != nil {
			return "", false
		}
	}
	// cur is now the final non-symlink target. Must be a regular,
	// executable file (a real java binary, not a shim shell script).
	if !fi.Mode().IsRegular() {
		return "", false
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	// Reject version-manager shim scripts (jenv/asdf/SDKMAN). A jenv
	// shim at ~/.jenv/shims/java is a regular executable shell SCRIPT
	// ("#!/bin/sh\nexec ..."), NOT a symlink and NOT a native binary.
	// realJava's regular+executable checks above are not enough to
	// distinguish a shell script from an ELF/Mach-O java: reading the
	// first few bytes is required. A shim starts with `#!`; a real java
	// starts with an ELF magic (0x7f 'E' 'L' 'F') on Linux or a Mach-O
	// magic on macOS (0xfeedface / 0xfeedfacf / 0xcafebabe fat). We
	// reject `#!` explicitly (covers every version-manager shim) and
	// accept any other non-script header — requiring a full native-
	// binary magic check would reject stub binaries in unit tests.
	if isShellScript(cur) {
		return "", false
	}
	return cur, true
}

// EnumerateHostJDKs discovers ALL real JDK installations on the host so
// Gradle's toolchain auto-detection can match a pinned toolchain spec
// (languageVersion + vendor) against an installed JDK WITHOUT relying
// on /usr/libexec/java_home inside the sandbox. The sandbox breaks
// java_home's LaunchServices/Spotlight-based enumeration (the binary
// runs but finds nothing — verified), so the supervisor runs java_home
// HERE, unsandboxed, and passes the discovered install roots to Gradle
// via org.gradle.java.installations.paths (read-only control state).
//
// Resolution: run `/usr/libexec/java_home -V` (darwin only), parse its
// stdout (one JDK per line: "<ver> (<arch>) \"<vendor>\" - \"<name>\" <path>").
// If java_home is unavailable or fails, fall back to a directory scan
// of the two macOS install locations (/Library/Java/JavaVirtualMachines
// and ~/Library/Java/JavaVirtualMachines), validating each entry has a
// real bin/java. Each returned path is the JDK Home (the parent of bin/).
//
// The returned roots are symlink-resolved and deduped. The daemon JDK
// (from ResolveJDK) is always included in the set so Gradle sees it as
// a toolchain candidate too. Returns nil on non-darwin (no java_home).
func EnumerateHostJDKs(daemonJDKHome string) []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	seen := map[string]bool{}
	var roots []string
	add := func(home string) {
		if home == "" {
			return
		}
		if canon, err := filepath.EvalSymlinks(home); err == nil {
			home = canon
		}
		if seen[home] {
			return
		}
		// Validate: bin/java must exist as a real executable.
		java := filepath.Join(home, "bin", "java")
		if _, ok := realJava(java); !ok {
			return
		}
		seen[home] = true
		roots = append(roots, home)
	}

	for _, home := range javaHomeListings() {
		add(home)
	}
	// The daemon JDK is always a valid toolchain candidate.
	add(daemonJDKHome)

	sort.Strings(roots)
	return roots
}

// javaHomeListings runs /usr/libexec/java_home -V unsandboxed and parses
// the stdout, then falls back to a directory scan if that fails. Each
// returned entry is a JDK Home path (parent of bin/).
func javaHomeListings() []string {
	if homes := parseJavaHomeV(); len(homes) > 0 {
		return homes
	}
	return scanJavaVirtualMachines()
}

// parseJavaHomeV execs /usr/libexec/java_home -V and parses the stdout.
// Each line looks like:
//
//	25.0.3 (arm64) "Eclipse Adoptium" - "OpenJDK 25.0.3" /Library/Java/JavaVirtualMachines/temurin-25.jdk/Contents/Home
//
// The path is the last whitespace-separated token on each line.
func parseJavaHomeV() []string {
	out, err := exec.Command(javaHomeBin, "-V").CombinedOutput()
	if err != nil {
		return nil
	}
	var roots []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Matching Java") || strings.HasPrefix(line, "The operation") {
			continue
		}
		// The path is the last token; it starts with "/".
		idx := strings.LastIndex(line, " /")
		if idx < 0 {
			continue
		}
		home := strings.TrimSpace(line[idx+1:])
		if home != "" {
			roots = append(roots, home)
		}
	}
	return roots
}

// scanJavaVirtualMachines scans the two macOS JDK install locations for
// *.jdk bundles with a real bin/java, returning the JDK Home of each.
// This is the fallback when java_home -V fails (e.g. LaunchServices DB
// corruption) and also covers JDKs java_home doesn't register.
func scanJavaVirtualMachines() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{
		"/Library/Java/JavaVirtualMachines",
		filepath.Join(home, "Library", "Java", "JavaVirtualMachines"),
	}
	var roots []string
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".jdk") {
				continue
			}
			home := filepath.Join(d, e.Name(), "Contents", "Home")
			if _, ok := realJava(filepath.Join(home, "bin", "java")); ok {
				roots = append(roots, home)
			}
		}
	}
	return roots
}

// javaHomeBin is the macOS JDK enumeration helper. Overridable for tests.
var javaHomeBin = "/usr/libexec/java_home"

// isShellScript reports whether path begins with a shebang ("#!"), the
// signature of a shell/script wrapper used by version-manager shims
// (jenv/asdf/SDKMAN). A real java binary never starts with `#!`. A read
// error is treated as "not a script" so the caller's regular+executable
// checks remain the gate; this only screens out script-form shims.
func isShellScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var hdr [2]byte
	n, err := f.Read(hdr[:])
	if err != nil || n < 2 {
		return false
	}
	return bytes.Equal(hdr[:], []byte("#!"))
}

// jdkReadPaths returns the bin + install-prefix support dirs (lib,
// libexec, lib64) for a JDK home, so Seatbelt can grant read+exec access
// for the JVM to exec and load native libs. Shared by the daemon JDK
// resolution (buildJDKResolution) and the toolchain JDK grants.
func jdkReadPaths(jdkHome string) []string {
	readPaths := []string{filepath.Join(jdkHome, "bin")}
	for _, name := range []string{"lib", "libexec", "lib64"} {
		p := filepath.Join(jdkHome, name)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			readPaths = append(readPaths, p)
		}
	}
	return readPaths
}

// buildJDKResolution assembles the corrected env + read-grants for a real
// JDK root, stripping shim dirs from the parent PATH and prepending the
// real bin. ReadPaths covers the bin dir and the install-prefix support
// dirs (lib/libexec) so the JVM can exec and load native libs under
// deny-default Seatbelt.
func buildJDKResolution(jdkHome, parentPath string) JDKResolution {
	binDir := filepath.Join(jdkHome, "bin")
	var kept []string
	for _, dir := range filepath.SplitList(parentPath) {
		if dir == "" {
			continue
		}
		if isShimDir(dir) {
			continue
		}
		kept = append(kept, dir)
	}
	correctedPath := binDir + string(filepath.ListSeparator) + strings.Join(kept, string(filepath.ListSeparator))

	return JDKResolution{
		JavaHome:  jdkHome,
		BinDir:    binDir,
		Path:      correctedPath,
		ReadPaths: jdkReadPaths(jdkHome),
	}
}

// shimMarkers are the path fragments that identify a version-manager tree
// (jenv, asdf, SDKMAN). A PATH entry whose RESOLVED path contains a marker
// is a shim-managed dir; stripping these prevents the child from trying to
// exec a shim that needs /dev/fd process substitution under the kernel
// sandbox. The bare-shims fallback (isShimDir) reuses the same marker set
// for its parent check rather than duplicating the manager names.
var shimMarkers = []string{"/.jenv/", "/.asdf/", "/.sdkman/", "/sdkman/candidates/"}

// isShimDir reports whether a PATH entry is a version-manager shim
// directory (jenv, asdf, SDKMAN). Symlinks to such dirs are detected by
// resolving first. Any entry passing through a version-manager tree is
// stripped (over-match is harmless: the real JDK bin is prepended
// separately after symlink resolution).
func isShimDir(dir string) bool {
	resolved := dir
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		resolved = r
	}
	lower := strings.ToLower(resolved)
	if pathMatchesShimMarker(lower) {
		return true
	}
	// Bare shim-dir fallback: a PATH entry that IS the shims dir (basename
	// shims/shims-bin) whose resolved path does not itself contain a
	// marker. Only treat it as a shim dir when its parent matches the same
	// marker set — /usr/shims is not a thing, ~/.jenv/shims is.
	base := filepath.Base(lower)
	if base == "shims" || base == "shims-bin" {
		return parentMatchesShimMarker(lower)
	}
	return false
}

// pathMatchesShimMarker reports whether a lower-cased path contains any
// shimMarkers fragment.
func pathMatchesShimMarker(lowerPath string) bool {
	for _, marker := range shimMarkers {
		if strings.Contains(lowerPath, marker) {
			return true
		}
	}
	return false
}

// parentMatchesShimMarker applies the marker set to a directory's parent.
// A trailing slash is appended: the markers are written in interior-slash
// form ("/.jenv/"), and a parent that ENDS at the manager root — the only
// shape that matters when dir itself is the manager's shims dir — would
// otherwise never match ("/home/u/.jenv" → "/home/u/.jenv/"). A filesystem
// root parent ("/") never matches, so a top-level "/shims" is not stripped.
func parentMatchesShimMarker(lowerDir string) bool {
	parent := filepath.Dir(lowerDir)
	if parent == "/" {
		return false
	}
	return pathMatchesShimMarker(parent + "/")
}

// String is for diagnostics only (never logged with secrets; JDK paths are
// not secret).
func (r JDKResolution) String() string {
	return fmt.Sprintf("JAVA_HOME=%s bin=%s path-prefix=%s read=%v", r.JavaHome, r.BinDir, r.BinDir, r.ReadPaths)
}
