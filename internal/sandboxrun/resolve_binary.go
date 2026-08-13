package sandboxrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// resolveInnerBinaryDirs resolves the inner command's executable on the
// host PATH and returns the directories that must be granted for it to be
// reachable inside the sandbox:
//
//   - the directory of the PATH entry itself, which is frequently a symlink
//     or shim (e.g. ~/.bun/bin/opencode, a mise/asdf shim). The sandbox
//     re-runs LookPath, so this dir must be on PATH or the lookup fails
//     even when the real binary is present elsewhere.
//   - the directory of its symlink-resolved real file (e.g.
//     ~/.bun/install/.../opencode-ai/bin/opencode.exe), so the link target
//     and its sibling files (shared libs, node runtime) are reachable.
//   - when the resolved file is a script with a shebang (e.g.
//     #!/usr/bin/env node), the interpreter's directory too — so the
//     kernel can exec the script inside the sandbox.
//   - the install-prefix support directories of each of the above, for
//     tools whose runtime lives beside the bin dir rather than in it
//     (see prefixSupportDirs).
//
// When the inner command is wrapped in an `env NAME=VALUE ... <cmd>` prefix
// (a sandbox profile may do this to set NPM_CONFIG_* etc. before launching
// the harness), the wrapped <cmd> is resolved and granted in addition to
// `env` itself — otherwise only `env`'s own directory would be granted and
// the harness (e.g. a bun/npm-installed opencode) would fail to exec.
//
// Returns nil when the command cannot be found or resolved.
func resolveInnerBinaryDirs(innerArgv []string) []string {
	dirs := resolveCommandBinaryDirs(innerArgv)
	if cmd := unwrapEnv(innerArgv); len(cmd) > 0 && cmd[0] != innerArgv[0] {
		dirs = append(dirs, resolveCommandBinaryDirs(cmd)...)
	}
	return dirs
}

// resolveCommandBinaryDirs is the core resolution for a single command argv
// (no env-wrapper handling): the directory of every hop of the symlink chain
// from the PATH entry to the real file, plus shebang interpreter dirs.
// Returns nil when argv is empty or unresolvable.
func resolveCommandBinaryDirs(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	resolved := lookPathAbs(argv[0])
	if resolved == "" {
		return nil
	}
	dirs := symlinkChainDirs(resolved)
	if real, rerr := filepath.EvalSymlinks(resolved); rerr == nil {
		if interp := shebangInterpreter(real); interp != "" {
			if idirs := resolveInterpreterDirs(interp); len(idirs) > 0 {
				dirs = append(dirs, idirs...)
			}
		}
	}
	return withPrefixSupportDirs(dirs)
}

// maxSymlinkHops bounds the chain walk. Well above any real install layout
// (Homebrew's deepest is three) and below the kernel's own ELOOP limit, so a
// cyclic or adversarial chain terminates here rather than spinning.
const maxSymlinkHops = 32

// symlinkChainDirs returns the directory of every hop of the symlink chain
// starting at path: the path itself, each intermediate link target, and the
// real file at the end. Duplicates are removed, order preserved.
//
// Granting only the two ENDS of the chain — which is what taking Dir(path)
// and Dir(EvalSymlinks(path)) amounts to — is not enough, because the kernel
// resolves the chain one hop at a time and needs read access to each link it
// reads on the way. Homebrew under a non-default prefix is the case that
// exposed it (issue #229): with the prefix outside every baseline grant,
//
//	<prefix>/bin/opencode
//	  -> <prefix>/Cellar/opencode/<v>/bin/opencode          <- ungranted hop
//	  -> <prefix>/Cellar/opencode/<v>/libexec/.../opencode.exe
//
// only the first and last directories were granted, so resolution died on the
// middle link. Measured under a profile granting just the two ends, all three
// argv[0] forms behave differently — the bare name fails ENOENT ("No such
// file or directory", naming a binary that is plainly on PATH), the absolute
// PATH-entry path fails EPERM, and only the fully resolved path runs. Adding
// the middle directory makes all three work, which is why this walks the
// chain instead of hardening argv[0] alone: passing an absolute path would
// only have moved the failure from ENOENT to EPERM.
//
// Note it is the DIRECTORIES that are returned, not the links: a subpath
// grant on the directory is what lets the kernel both read the link and stat
// its siblings, matching how the rest of this file grants access.
func symlinkChainDirs(path string) []string {
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	cur := path
	for range maxSymlinkHops {
		add(filepath.Dir(cur))
		fi, err := os.Lstat(cur)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			break
		}
		target, terr := os.Readlink(cur)
		if terr != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		cur = filepath.Clean(target)
	}
	// A symlinked ancestor (e.g. a version dir that is itself a link) means
	// the kernel matches the resolved spelling, which the per-hop walk above
	// never produces. EvalSymlinks on the end of the chain closes that gap;
	// it is a no-op for the common all-real-directories case.
	if real, rerr := filepath.EvalSymlinks(path); rerr == nil {
		add(filepath.Dir(real))
	}
	return dirs
}

// lookPathAbs resolves name against the host PATH and returns it as an
// absolute path, WITHOUT following symlinks. Returns "" when the command
// cannot be resolved.
//
// Not following symlinks is deliberate, for two reasons.
//
// The link itself is what a PATH lookup finds, and shim-style installs
// (Homebrew's <prefix>/bin, mise/asdf) rely on the harness seeing the link
// path rather than the store path behind it.
//
// More importantly, resolving would silently defeat the profile. Under a
// deny covering the harness's tree (e.g. filesystem.deny: ["~"] with a
// $HOME-rooted Homebrew prefix) all three argv[0] forms were measured:
// the bare name fails ENOENT, this absolute link path fails EPERM, and
// the EvalSymlinks-resolved path RUNS — even though the resolved file is
// itself inside the denied tree and reading it is refused (cat → EPERM).
// It runs only because exec of a main image consults (allow process-exec*)
// and never the file-read rules, whereas resolving a symlink must read the
// link in the denied directory. So the resolved form succeeds by bypassing
// the user's stated policy, not by being more correct. omac reports that
// conflict instead — see protectedInnerBinaryWarning. Linux's backend does
// resolve (backend_linux.go), which gives it the same bypass property;
// that inconsistency wants fixing on the Linux side, not copying here.
func lookPathAbs(name string) string {
	if name == "" {
		return ""
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	if abs, aerr := filepath.Abs(resolved); aerr == nil {
		return abs
	}
	return resolved
}

// absoluteInnerArgv returns innerArgv with every executable token replaced
// by the absolute path the HOST PATH lookup resolves it to: argv[0], and —
// when argv is an `env NAME=VALUE ... <cmd>` wrapper — the wrapped <cmd>
// as well.
//
// Without this the harness is resolved twice: once here, on the host, to
// compute the sandbox grants (resolveInnerBinaryDirs), and a second time
// INSIDE the sandbox, because a bare command name makes the PATH search
// happen there — by sandbox-exec's execvp for argv[0], or by `env` itself
// for the wrapped command. Whenever that second lookup fails, Seatbelt
// reports it as ENOENT, so the user is told
//
//	sandbox-exec: execvp() of 'opencode' failed: No such file or directory
//
// on a machine where `which opencode` resolves fine — an error naming
// neither the real cause nor the path involved.
//
// Passing absolute paths removes the second lookup entirely, which makes
// the launch independent of the child's PATH. That matters concretely:
// deny_vars is applied last and wins over BaseAllowVars, so a profile can
// strip PATH from the child, after which execvp falls back to libc's
// /usr/bin:/bin and cannot find the harness at all. An absolute argv[0]
// is the only form that survives it. Linux has never had the exposure —
// backend_linux.go rewrites argv[0] for its own reasons — so this is
// platform parity as well.
//
// The env-wrapper form is not hypothetical: a launcher profile may set
// NPM_CONFIG_* before launching the harness, in which case argv[0] is
// `env` and rewriting it alone would leave the wrapped harness lookup
// exactly as exposed as before.
//
// Tokens that cannot be resolved are left alone, so the caller's error
// surface is unchanged.
func absoluteInnerArgv(innerArgv []string) []string {
	out := append([]string(nil), innerArgv...)
	idx := []int{0}
	if cmd := envWrapperEnd(out); cmd > 0 {
		idx = append(idx, cmd)
	}
	for _, i := range idx {
		if i >= len(out) {
			continue
		}
		if abs := lookPathAbs(out[i]); abs != "" {
			out[i] = abs
		}
	}
	return out
}

// unwrapEnv strips a leading `env NAME=VALUE ...` wrapper and returns the
// argv that begins with the real command. It skips `env` and any following
// NAME=VALUE assignment tokens; the first non-assignment token is the
// command. `env` flags (e.g. -i, -u) are not used by omac's profiles and are
// left in place, so an argv like `env -i cmd` yields `-i cmd` — which fails
// to resolve and is safely ignored by the caller. Returns argv unchanged when
// there is no env wrapper.
func unwrapEnv(argv []string) []string {
	return argv[envWrapperEnd(argv):]
}

// envWrapperEnd returns the index at which the real command begins after any
// leading `env NAME=VALUE ...` wrapper — 0 when there is none. Split out of
// unwrapEnv so callers that must REWRITE the wrapped command token (rather
// than just read it) know where it sits; see absoluteInnerArgv.
func envWrapperEnd(argv []string) int {
	i := 0
	for len(argv)-i >= 2 && filepath.Base(argv[i]) == "env" {
		j := i + 1
		for j < len(argv) && isEnvAssignment(argv[j]) {
			j++
		}
		i = j
	}
	return i
}

// isEnvAssignment reports whether tok is a NAME=VALUE assignment (not a flag).
func isEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	return eq > 0 && !strings.HasPrefix(tok, "-")
}

// unresolvedInnerCommandWarning returns the operator-facing warning for an
// inner command that cannot be found on the host PATH, or "" when it can.
//
// Resolution failure is otherwise silent — resolveCommandBinaryDirs just
// returns nil — and the launch proceeds to fail deep inside the sandbox
// with an error that names the wrong cause (ENOENT on a command the user
// can see on their PATH). Saying it once, up front, at the point where
// the host lookup happened, is the difference between a one-line fix and
// a Seatbelt investigation.
func unresolvedInnerCommandWarning(innerArgv []string) string {
	cmd := unwrapEnv(innerArgv)
	if len(cmd) == 0 || cmd[0] == "" {
		return ""
	}
	if lookPathAbs(cmd[0]) != "" {
		return ""
	}
	return "omac sandbox: warning: " + cmd[0] + " is not on PATH as seen by this process — " +
		"the sandbox cannot be granted access to it and the launch will fail with " +
		"\"No such file or directory\". Check that the directory holding it is on PATH here, " +
		"not only in your interactive shell."
}

// protectedInnerBinaryWarning returns the operator-facing warning for a
// protected-path deny that covers a directory the inner command must be read
// from, or "" when none does.
//
// Protected denies are emitted AFTER the read allows and Seatbelt is
// last-match-wins (GenerateSBPL), so a deny over a tree containing the
// harness defeats the per-launch grant this package just computed for it.
// The user sees only
//
//	sandbox-exec: execvp() of 'opencode' failed: No such file or directory
//
// which names neither the deny nor the directory. A deny broad enough to
// swallow the harness is nearly always a mistake rather than intent — but it
// IS the user's stated policy, so this reports the conflict instead of
// quietly routing around it. Silently resolving the binary out of the denied
// tree would launch it in defiance of the profile, which is not omac's call
// to make in a security boundary.
func protectedInnerBinaryWarning(innerArgv []string, protected []string) string {
	for _, dir := range resolveInnerBinaryDirs(innerArgv) {
		for _, p := range protected {
			if pathCoveredBy(dir, p) {
				return "omac sandbox: warning: this profile denies " + p +
					", which contains the harness binary directory " + dir + ". The deny is applied" +
					" after the grants, so the harness cannot be read and the launch will fail with" +
					" \"No such file or directory\". Narrow the deny (filesystem.deny / --deny) or" +
					" move the harness outside it."
			}
		}
	}
	return ""
}

// pathCoveredBy reports whether path lies at or under root, comparing every
// combination of their literal and symlink-resolved forms.
//
// Both sides need canonicalizing: a deny is stored as the user wrote it while
// a binary dir arrives from EvalSymlinks (or vice versa), so a single-sided
// comparison misses a match whenever either has a symlinked ancestor —
// /var/folders vs /private/var/folders being the everyday case. Seatbelt
// itself matches on the resolved form, so this mirrors what the kernel will
// actually do rather than what the strings happen to look like.
func pathCoveredBy(path, root string) bool {
	forms := func(p string) []string {
		out := []string{filepath.Clean(p)}
		if c := canonicalPath(p); c != "" && c != out[0] {
			out = append(out, c)
		}
		return out
	}
	for _, d := range forms(path) {
		for _, r := range forms(root) {
			if d == r || strings.HasPrefix(d, r+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// resolveInnerBinaryPath resolves the inner command's executable to its
// real absolute path (following all symlinks). Used by Linux to rewrite
// argv[0] so stage2 execs the real binary directly.
//
// Returns the original argv[0] when resolution fails.
func resolveInnerBinaryPath(innerArgv []string) string {
	if len(innerArgv) == 0 || innerArgv[0] == "" {
		return ""
	}
	resolved, err := exec.LookPath(innerArgv[0])
	if err != nil {
		return innerArgv[0]
	}
	if real, rerr := filepath.EvalSymlinks(resolved); rerr == nil {
		return real
	}
	if abs, aerr := filepath.Abs(resolved); aerr == nil {
		return abs
	}
	return innerArgv[0]
}

// shebangInterpreter reads the first line of path and, if it is a shebang
// (#!), returns the interpreter. Handles two forms:
//   - #!/usr/bin/env node       → "node"
//   - #!/usr/bin/node           → "/usr/bin/node"
//
// Returns "" when the file is not a script or has no shebang.
func shebangInterpreter(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	hdr := make([]byte, 256)
	n, _ := f.Read(hdr)
	line := string(hdr[:n])
	if !strings.HasPrefix(line, "#!") {
		return ""
	}
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "#!"))
	if line == "" {
		return ""
	}
	parts := strings.Fields(line)
	interp := parts[0]
	if filepath.Base(interp) == "env" && len(parts) > 1 {
		for _, p := range parts[1:] {
			if !strings.HasPrefix(p, "-") {
				return p
			}
		}
		return ""
	}
	return interp
}

// resolveInterpreterDirs resolves an interpreter (bare name like "node" or
// absolute path like "/usr/bin/node") on the host and returns the dirs to
// grant: the interpreter's dir and its symlink-resolved dir.
func resolveInterpreterDirs(interp string) []string {
	resolved, err := exec.LookPath(interp)
	if err != nil {
		if filepath.IsAbs(interp) {
			if _, err := os.Stat(interp); err == nil {
				return withPrefixSupportDirs([]string{filepath.Dir(interp)})
			}
		}
		return nil
	}
	if abs, aerr := filepath.Abs(resolved); aerr == nil {
		resolved = abs
	}
	// Chain-walked for the same reason the harness itself is: an interpreter
	// reached through a version manager (nvm, mise, asdf) is a multi-hop
	// symlink, and a middle hop left ungranted fails the exec of every script
	// that names it — see symlinkChainDirs.
	return withPrefixSupportDirs(symlinkChainDirs(resolved))
}

// prefixBinDirNames are the conventional executable subdirectories of a Unix
// install prefix: a binary in one of these identifies its parent as a prefix.
var prefixBinDirNames = []string{"bin", "sbin", "libexec"}

// prefixSupportDirNames are the sibling subdirectories of an install prefix
// that hold a tool's runtime support files. Deliberately narrow: only trees a
// binary loads at runtime, never data/config trees like share or etc.
var prefixSupportDirNames = []string{"lib", "lib64", "libexec", "conf"}

// prefixSupportDirs returns the existing install-prefix support directories
// for a binary living in binDir.
//
// A prefix-layout tool loads its runtime from beside its bin dir, not from
// inside it, so granting only the bin dir leaves it unable to start. A JDK is
// the canonical case: bin/java needs lib/libjli.so to exec at all and reads
// conf/security/java.security before it can open a TLS connection. The same
// shape covers Python venvs, version-managed toolchains (mise/asdf/sdkman),
// and relocatable tarball installs — anything whose root is not already
// covered by a baseline grant such as /usr.
//
// The prefix itself is never granted, only the support subdirs, so sibling
// trees (src, share, a checkout) stay invisible. $HOME and its ancestors are
// never treated as a prefix: a personal ~/bin is not a self-contained install
// tree, and deriving one would widen the grant to ~/lib off the back of any
// binary the user drops there.
func prefixSupportDirs(binDir string) []string {
	if !slices.Contains(prefixBinDirNames, filepath.Base(binDir)) {
		return nil
	}
	prefix := filepath.Dir(binDir)
	if !isInstallPrefix(prefix) {
		return nil
	}
	var out []string
	for _, name := range prefixSupportDirNames {
		p := filepath.Join(prefix, name)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// isInstallPrefix reports whether prefix may be treated as a tool's install
// root. The filesystem root, the user's home, and any ancestor of it are
// rejected — see prefixSupportDirs.
func isInstallPrefix(prefix string) bool {
	if prefix == "" || prefix == "/" || prefix == filepath.Dir(prefix) {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return true
	}
	if abs, aerr := filepath.Abs(home); aerr == nil {
		home = abs
	}
	for d := filepath.Clean(home); ; d = filepath.Dir(d) {
		if d == prefix {
			return false
		}
		if d == filepath.Dir(d) {
			return true
		}
	}
}

// withPrefixSupportDirs appends each dir's install-prefix support dirs,
// preserving order and dropping duplicates. Nil in, nil out — callers
// distinguish "nothing to grant" from an empty grant list.
func withPrefixSupportDirs(dirs []string) []string {
	if len(dirs) == 0 {
		return nil
	}
	out := make([]string, 0, len(dirs))
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, d := range dirs {
		add(d)
	}
	for _, d := range dirs {
		for _, s := range prefixSupportDirs(d) {
			add(s)
		}
	}
	return out
}
