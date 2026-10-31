package sandboxrun

import (
	"errors"
	"fmt"
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
// pol bounds what may be derived: every directory the resolution grants and
// every path it visits on the way is checked against it (see
// derivedGrantPolicy), so an inner command the confined process can rewrite
// cannot extend the next sandbox's read surface. The zero policy performs
// no checks — the backends pass one derived from the grants.
//
// Returns nil when the command cannot be found or resolved. An error means
// the resolution crossed the policy and the launch must fail closed.
func resolveInnerBinaryDirs(innerArgv []string, pol derivedGrantPolicy) ([]string, error) {
	dirs, err := commandBinaryDirs(innerArgv, pol)
	if err != nil {
		return nil, err
	}
	if cmd := UnwrapEnv(innerArgv); len(cmd) > 0 && cmd[0] != innerArgv[0] {
		wrapped, werr := commandBinaryDirs(cmd, pol)
		if werr != nil {
			return nil, werr
		}
		dirs = append(dirs, wrapped...)
	}
	return dirs, nil
}

// resolveCommandBinaryDirs is the unbounded form of commandBinaryDirs for
// callers that only inspect the resolution (diagnostics, tests): the
// policy is the zero value, so nothing is refused. Returns nil when argv
// is empty or unresolvable.
func resolveCommandBinaryDirs(argv []string) []string {
	dirs, _ := commandBinaryDirs(argv, derivedGrantPolicy{})
	return dirs
}

// commandBinaryDirs is the core resolution for a single command argv
// (no env-wrapper handling): the directory of every hop of the symlink chain
// from the PATH entry to the real file, plus shebang interpreter dirs.
// Returns nil when argv is empty or unresolvable. pol bounds the
// derivation; an error means it crossed the policy.
func commandBinaryDirs(argv []string, pol derivedGrantPolicy) ([]string, error) {
	if len(argv) == 0 {
		return nil, nil
	}
	resolved := lookPathAbs(argv[0])
	if resolved == "" {
		return nil, nil
	}
	dirs, hops := walkSymlinkChain(resolved)
	if real, rerr := filepath.EvalSymlinks(resolved); rerr == nil {
		if interp := shebangInterpreter(real); interp != "" {
			idirs, ihops := resolveInterpreterChain(interp)
			dirs = append(dirs, idirs...)
			hops = append(hops, ihops...)
		}
	}
	dirs = withPrefixSupportDirs(dirs)
	if err := refuseDerivedGrants(dirs, hops, pol); err != nil {
		return nil, err
	}
	return dirs, nil
}

// maxSymlinkHops bounds the chain walk. Well above any real install layout
// (Homebrew's deepest is three) and below the kernel's own ELOOP limit, so a
// cyclic or adversarial chain terminates here rather than spinning.
const maxSymlinkHops = 32

// derivedGrantPolicy bounds what a per-launch derivation may grant. The
// inner command's resolution (commandBinaryDirs) hands every candidate
// directory and every path it visited to refuseDerivedGrants, which applies
// the two rules below.
//
// The zero policy performs no checks: legacy callers that only inspect a
// resolution (protectedInnerBinaryWarning, tests) have no grants to compare
// against. The backends pass one built from the grants, which always has
// at least the shared temp roots as writable.
type derivedGrantPolicy struct {
	// writable holds the roots the confined process can change: write and
	// allow grants plus the shared temp roots. A command, symlink hop, or
	// shebang interpreter visited there is tainted: its content at launch
	// time is whatever the agent last wrote, so it must not pick what the
	// sandbox grants read access to.
	writable []string
	// granted holds the paths the profile already allows explicitly.
	// Deriving a directory covered by them adds nothing the user has not
	// already granted, so only the remainder (the extension) is subject
	// to the rules.
	granted []string
	// homes holds the home directories a derivation must not silently
	// extend into; see homeSensitiveDirs.
	homes []string
}

// derivedGrantPolicyForGrants builds the bounding policy from a grant set.
// The workdir is deliberately NOT treated as writable on its own: a
// read-only workdir cannot host a rewritten command, and ResolveGrants
// already maps writable workdirs into AllowPaths.
func derivedGrantPolicyForGrants(g *Grants) derivedGrantPolicy {
	pol := derivedGrantPolicy{
		writable: append(append([]string{}, g.WritePaths...), g.AllowPaths...),
		granted:  append(append(append([]string{}, g.ReadPaths...), g.WritePaths...), g.AllowPaths...),
	}
	pol.writable = append(pol.writable, sharedTempRoots()...)
	userHome, uerr := os.UserHomeDir()
	if uerr != nil {
		userHome = ""
	}
	for _, h := range []string{g.HomeDir, userHome} {
		if h != "" && !slices.Contains(pol.homes, h) {
			h = filepath.Clean(h)
			if !slices.Contains(pol.homes, h) {
				pol.homes = append(pol.homes, h)
			}
		}
	}
	return pol
}

// sharedTempRoots returns the shared temp locations: the directory the
// process was configured with (TMPDIR and friends) plus the conventional
// defaults. Every process on the host can typically write there, so they
// count as agent-writable surface.
func sharedTempRoots() []string {
	var roots []string
	for _, r := range []string{os.TempDir(), "/tmp", "/var/tmp"} {
		if r == "" {
			continue
		}
		r = filepath.Clean(r)
		if !slices.Contains(roots, r) {
			roots = append(roots, r)
		}
	}
	return roots
}

// homeSensitiveDirs are the trees below a home directory that hold
// credentials or personal config: deriving read access into them off the
// back of a command resolution would expose secrets the profile never
// granted. Dot-cache is deliberately absent: version managers (mise and
// friends) keep their runtimes there, and those must keep resolving.
var homeSensitiveDirs = []string{
	".config", ".ssh", ".gnupg", ".aws", ".azure", ".gcloud", ".kube",
	".docker", ".pki", ".terraform.d", ".credentials", ".secrets", ".keys",
	".password-store", ".1password",
}

// refuseDerivedGrants applies the derivedGrantPolicy to a resolution: dirs
// is what the resolution wants to grant, hops is every path it visited on
// the way (the command file itself, each symlink target, each shebang
// interpreter).
//
// Two rules, in order:
//
//   - Taint: a hop inside agent-writable surface means the command's
//     content is attacker-chosen. Granting anything NEW on its say-so
//     would let the agent pick what the sandbox reads, so the launch fails
//     when the derivation extends past the explicit grants. It is the
//     extension that is refused, not the derivation: a tainted command
//     whose every dir is already granted stays launchable, because
//     nothing new is exposed.
//   - Home boundary: a NEW dir below a home's sensitive trees is refused
//     even for untainted commands. A user's config root must be granted
//     by the user, not derived.
//
// A nil policy (no writable surface, no homes) skips both rules.
func refuseDerivedGrants(dirs, hops []string, pol derivedGrantPolicy) error {
	if len(pol.writable) == 0 && len(pol.homes) == 0 {
		return nil
	}
	var ext []string
	for _, d := range dirs {
		if !pathCoveredByAny(d, pol.granted) {
			ext = append(ext, d)
		}
	}
	if len(ext) == 0 {
		return nil
	}
	for _, hop := range hops {
		if !pathCoveredByAny(hop, pol.writable) {
			continue
		}
		var b strings.Builder
		b.WriteString("omac can't start the sandbox: the inner command lives where the agent can write\n" +
			"(the workdir or a temp dir), and launching it would have to grant read access to\n" +
			"paths the profile does not already allow:\n")
		for _, d := range ext {
			fmt.Fprintf(&b, "  %s\n", d)
		}
		b.WriteString("\n" +
			"The command's content at launch time is whatever the agent last wrote there, so\n" +
			"launching it must not choose what the sandbox can read. Move the command outside\n" +
			"the agent-writable area, or grant the paths it needs explicitly (filesystem.read /\n" +
			"--read).")
		return errors.New(b.String())
	}
	for _, d := range ext {
		if home, sensitive := sensitiveUnderHome(d, pol.homes); sensitive {
			//lint:ignore ST1005 multi-paragraph user-facing message, punctuation is intentional
			return fmt.Errorf(
				"omac can't start the sandbox: launching this command would derive read access\n"+
					"to %s\n"+
					"below the home directory %s, in a tree that holds config or credentials. omac\n"+
					"only derives access there when you grant it explicitly (filesystem.read / --read)\n"+
					"or move the tool outside the home tree.", d, home)
		}
	}
	return nil
}

// sensitiveUnderHome reports whether dir sits below one of a home
// directory's sensitive trees, returning that home when it does.
func sensitiveUnderHome(dir string, homes []string) (string, bool) {
	for _, home := range homes {
		rel, err := filepath.Rel(home, dir)
		if err != nil || rel == ".." || rel == "." ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		first := rel
		if i := strings.IndexByte(rel, filepath.Separator); i >= 0 {
			first = rel[:i]
		}
		if slices.Contains(homeSensitiveDirs, first) {
			return home, true
		}
	}
	return "", false
}

// pathCoveredByAny reports whether path lies at or under any of roots,
// comparing every combination of literal and symlink-resolved forms.
func pathCoveredByAny(path string, roots []string) bool {
	for _, r := range roots {
		if pathCoveredBy(path, r) {
			return true
		}
	}
	return false
}

// walkSymlinkChain walks the symlink chain starting at path and returns two
// lists: the directory of every hop (what the sandbox must be granted), and
// every path it visited on the way (the path itself, each intermediate link
// target, and the real file the chain ends at). The visited paths feed the
// derived-grant taint check: content the agent can rewrite at any hop makes
// the whole derivation untrustworthy. Duplicates removed, order preserved.
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
// Note the first list holds the DIRECTORIES, not the links: a subpath
// grant on the directory is what lets the kernel both read the link and stat
// its siblings, matching how the rest of this file grants access.
func walkSymlinkChain(path string) (dirs, hops []string) {
	seenDirs := map[string]bool{}
	seenHops := map[string]bool{}
	addDir := func(d string) {
		if !seenDirs[d] {
			seenDirs[d] = true
			dirs = append(dirs, d)
		}
	}
	addHop := func(p string) {
		if !seenHops[p] {
			seenHops[p] = true
			hops = append(hops, p)
		}
	}
	cur := path
	for range maxSymlinkHops {
		addHop(cur)
		addDir(filepath.Dir(cur))
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
		addHop(real)
		addDir(filepath.Dir(real))
	}
	return dirs, hops
}

// symlinkChainDirs is walkSymlinkChain's directory list alone, for callers
// that only inspect the grant surface (diagnostics, tests) and have no
// policy to check the visited paths against.
func symlinkChainDirs(path string) []string {
	dirs, _ := walkSymlinkChain(path)
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

// UnwrapEnv strips a leading `env NAME=VALUE ...` wrapper and returns the
// argv that begins with the real command: it skips `env` and any following
// NAME=VALUE assignment tokens. `env` flags (e.g. -i, -u) are left in place,
// so `env -i cmd` yields `-i cmd`. Returns argv unchanged when there is no
// wrapper.
func UnwrapEnv(argv []string) []string {
	return argv[envWrapperEnd(argv):]
}

// envWrapperEnd returns the index at which the real command begins after any
// leading `env NAME=VALUE ...` wrapper — 0 when there is none. Split out of
// UnwrapEnv so callers that must REWRITE the wrapped command token (rather
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
	cmd := UnwrapEnv(innerArgv)
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
//
// The zero policy keeps this a pure inspection: it has no grant set to check
// against, and the launch-time enforcement happens in the backends.
func protectedInnerBinaryWarning(innerArgv []string, protected []string) string {
	dirs, _ := resolveInnerBinaryDirs(innerArgv, derivedGrantPolicy{})
	for _, dir := range dirs {
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

// resolveInterpreterChain resolves an interpreter (bare name like "node" or
// absolute path like "/usr/bin/node") on the host and returns the dirs to
// grant: the interpreter's dir and its symlink-resolved dir. The second list
// holds every path the resolution visited — the interpreter file, each
// symlink hop to it, the real file at the end — which feeds the derived-grant
// taint check (see derivedGrantPolicy).
func resolveInterpreterChain(interp string) (dirs, hops []string) {
	resolved, err := exec.LookPath(interp)
	if err != nil {
		if filepath.IsAbs(interp) {
			if _, serr := os.Stat(interp); serr == nil {
				hops = []string{interp}
				d := filepath.Dir(interp)
				if isBroadGrant(d) {
					return nil, hops
				}
				return withPrefixSupportDirs([]string{d}), hops
			}
		}
		return nil, nil
	}
	if abs, aerr := filepath.Abs(resolved); aerr == nil {
		resolved = abs
	}
	// Chain-walked for the same reason the harness itself is: an interpreter
	// reached through a version manager (nvm, mise, asdf) is a multi-hop
	// symlink, and a middle hop left ungranted fails the exec of every script
	// that names it — see walkSymlinkChain.
	idirs, ihops := walkSymlinkChain(resolved)
	return withPrefixSupportDirs(idirs), ihops
}

// resolveInterpreterDirs is resolveInterpreterChain's directory list alone,
// for callers that only inspect the grant surface (diagnostics, tests) and
// have no policy to check the visited paths against.
func resolveInterpreterDirs(interp string) []string {
	dirs, _ := resolveInterpreterChain(interp)
	return dirs
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

// isBroadGrant reports whether granting dir as a readable directory would
// expose an unacceptably wide subtree. "/" and $HOME (and its ancestors) are
// rejected; normal install-tree dirs (e.g. /usr/bin) are not.
func isBroadGrant(dir string) bool {
	if dir == "" || dir == "/" || dir == filepath.Dir(dir) {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	if abs, aerr := filepath.Abs(home); aerr == nil {
		home = abs
	}
	// Reject $HOME itself and any ancestor of it (e.g. /Users or /).
	for d := filepath.Clean(home); ; d = filepath.Dir(d) {
		if d == dir {
			return true
		}
		if d == filepath.Dir(d) {
			return false
		}
	}
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
