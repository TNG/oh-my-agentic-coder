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
// no checks — the backends pass one derived from the grants. A bounded
// resolution returns only new, resolved targets; already-granted paths are
// omitted rather than re-emitting aliases that may have changed by lowering.
//
// Returns nil when the command cannot be found or resolved. An error means
// the resolution crossed the policy and the launch must fail closed.
func resolveInnerBinaryDirs(innerArgv []string, pol derivedGrantPolicy) ([]string, error) {
	dirs, _, err := resolveInnerBinaryPlan(innerArgv, pol)
	return dirs, err
}

// derivedReadDir keeps authorization identity separate from a pathname still
// used by env, shebangs or runtime support lookups. Linux may need both names
// in its namespace; real is never replaced by a later lookup of path.
type derivedReadDir struct {
	path string
	real string
}

func resolveInnerBinaryPlan(innerArgv []string, pol derivedGrantPolicy) ([]string, []derivedReadDir, error) {
	dirs, names, err := commandBinaryPlan(innerArgv, pol)
	if err != nil {
		return nil, nil, err
	}
	if cmd := UnwrapEnv(innerArgv); len(cmd) > 0 && cmd[0] != innerArgv[0] {
		wrapped, wrappedNames, werr := commandBinaryPlan(cmd, pol)
		if werr != nil {
			return nil, nil, werr
		}
		dirs = append(dirs, wrapped...)
		names = append(names, wrappedNames...)
	}
	return dirs, names, nil
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
	dirs, _, err := commandBinaryPlan(argv, pol)
	return dirs, err
}

func commandBinaryPlan(argv []string, pol derivedGrantPolicy) ([]string, []derivedReadDir, error) {
	if len(argv) == 0 {
		return nil, nil, nil
	}
	resolved := lookPathAbs(argv[0])
	if resolved == "" {
		return nil, nil, nil
	}
	real, dirs, hops, err := walkSymlinkPath(resolved, false)
	if err != nil {
		return nil, nil, err
	}
	if interp := shebangInterpreter(real); interp != "" {
		idirs, ihops, err := resolveInterpreterChain(interp)
		if err != nil {
			return nil, nil, err
		}
		dirs = append(dirs, idirs...)
		hops = append(hops, ihops...)
	}
	dirs, supportHops, err := resolvePrefixSupportDirs(dirs)
	if err != nil {
		return nil, nil, err
	}
	hops = append(hops, supportHops...)
	var names []derivedReadDir
	if len(pol.writable) != 0 || len(pol.homes) != 0 {
		// Admission uses targets, never the literal aliases used for deny
		// matching. Return only new, resolved grants: a covered mutable alias
		// must not be followed again when a backend lowers the policy.
		var targets []string
		for _, d := range dirs {
			real, _, dhops, err := walkSymlinkPath(d, true)
			if err != nil {
				return nil, nil, fmt.Errorf("resolve derived directory %q: %w", d, err)
			}
			hops = append(hops, dhops...)
			names = append(names, derivedReadDir{path: d, real: real})
			if !slices.Contains(targets, real) {
				targets = append(targets, real)
			}
		}
		dirs = targets
	}
	if err := refuseDerivedGrants(dirs, hops, pol); err != nil {
		return nil, nil, err
	}
	if len(pol.writable) != 0 || len(pol.homes) != 0 {
		var needed []string
		for _, d := range dirs {
			if !targetCoveredByAny(d, pol.readable) {
				needed = append(needed, d)
			}
		}
		dirs = needed
	}
	return dirs, names, nil
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
	// granted holds explicitly allowed path roots. Writable content may
	// only derive read access within them; the remainder is an extension
	// subject to the rules.
	granted []string
	// readable excludes write-only grants: they bound derivation, but on
	// macOS a command there still needs a new read rule to remain runnable.
	readable []string
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
		readable: append([]string{}, g.AllowPaths...),
	}
	// Linux deliberately does not bind root read grants. Keep a checked
	// derived directory for that case so its lowerer can mount the directory
	// that the command actually needs. Root allow is different: it is bound.
	for _, p := range g.ReadPaths {
		if filepath.Clean(p) != "/" {
			pol.readable = append(pol.readable, p)
		}
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
// Library is the macOS user tree: Application Support holds app tokens
// and keychain material. The home directory itself never sits in this
// list because sensitiveUnderHome treats it as sensitive directly. All
// other first-level dirs (~/bin, ~/homebrew, unlisted dotdirs) stay
// derivable: they are legitimate shim locations.
var homeSensitiveDirs = []string{
	".config", ".ssh", ".gnupg", ".aws", ".azure", ".gcloud", ".kube",
	".docker", ".pki", ".terraform.d", ".credentials", ".secrets", ".keys",
	".password-store", ".1password", "Library",
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
//   - Home boundary: a NEW dir that contains a home or sits below one
//     of its sensitive trees is refused even for untainted commands. A
//     user's home and its config/credential roots must be granted by the
//     user, not derived.
//
// A nil policy (no writable surface, no homes) skips both rules.
func refuseDerivedGrants(dirs, hops []string, pol derivedGrantPolicy) error {
	if len(pol.writable) == 0 && len(pol.homes) == 0 {
		return nil
	}
	var ext []string
	for _, d := range dirs {
		if !targetCoveredByAny(d, pol.granted) {
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
					"overlapping the home tree %s, which holds config or credentials. omac only\n"+
					"derives access there when you grant it explicitly (filesystem.read / --read)\n"+
					"or move the tool outside the home tree.", d, home)
		}
	}
	return nil
}

// sensitiveUnderHome reports whether dir contains a home or lies below one
// of its sensitive trees, returning that home when it does.
// Both sides are compared in their literal and symlink-resolved
// spellings: a derived dir can arrive from EvalSymlinks, while a home can
// be spelled the way the user wrote it.
func sensitiveUnderHome(dir string, homes []string) (string, bool) {
	for _, home := range homes {
		for _, hf := range pathForms(home) {
			for _, df := range pathForms(dir) {
				if sensitiveBelowHome(df, hf) {
					return home, true
				}
			}
		}
	}
	return "", false
}

// sensitiveBelowHome checks recursive grant overlap. An ancestor of home
// exposes its sensitive descendants just as granting home itself does.
func sensitiveBelowHome(dir, home string) bool {
	if pathWithin(home, dir) {
		return true
	}
	for _, name := range homeSensitiveDirs {
		for _, sensitive := range pathForms(filepath.Join(home, name)) {
			if pathWithin(dir, sensitive) || pathWithin(sensitive, dir) {
				return true
			}
		}
	}
	return false
}

// pathWithin handles root without a double-separator prefix and recognizes
// case aliases by filesystem identity, not by folding strings. Walking the
// existing ancestors also handles aliases of HOME, while preserving distinct
// .config and .CONFIG directories on case-sensitive volumes.
func pathWithin(path, root string) bool {
	if isUnder(filepath.Clean(path), filepath.Clean(root)) {
		return true
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if info, err := os.Stat(p); err == nil && os.SameFile(info, rootInfo) {
			return true
		}
		if p == filepath.Dir(p) {
			return false
		}
	}
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

// targetCoveredByAny is grant containment, not conservative deny matching:
// an alias below a granted root does not grant its out-of-tree target.
func targetCoveredByAny(path string, roots []string) bool {
	for _, root := range roots {
		real, err := filepath.EvalSymlinks(root)
		if err == nil && pathWithin(path, real) {
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
	_, dirs, hops, _ = walkSymlinkPath(path, false)
	return dirs, hops
}

// walkSymlinkPath resolves every component, not just the final filename.
// An ancestor link can lead through writable surface before reaching an
// otherwise trusted endpoint. Never fall back to an endpoint-only lookup.
// directory selects whether path itself or its parent needs a read grant.
func walkSymlinkPath(path string, directory bool) (real string, dirs, hops []string, err error) {
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
	addGrant := func(p string) {
		if directory {
			addDir(p)
		} else {
			addDir(filepath.Dir(p))
		}
	}
	addGrant(path)
	addHop(path)
	cur := string(filepath.Separator)
	pending := strings.Split(path, string(filepath.Separator))
	nlinks := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, part)
		addHop(next)
		fi, lerr := os.Lstat(next)
		if lerr != nil {
			return "", dirs, hops, fmt.Errorf("resolve command dependency %q: %w", next, lerr)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if len(pending) == 0 {
			addGrant(next)
		}
		nlinks++
		if nlinks > maxSymlinkHops {
			return "", dirs, hops, fmt.Errorf("resolve command dependency %q: more than %d symlink hops", path, maxSymlinkHops)
		}
		target, rerr := os.Readlink(next)
		if rerr != nil {
			return "", dirs, hops, fmt.Errorf("read command dependency %q: %w", next, rerr)
		}
		// The kernel continues from the link target, not just from the final
		// resolved file. Keep that spelling too: a target such as
		// ../current/bin/tool needs current/bin in the namespace even when
		// current resolves to versions/v1 on the host.
		spelling := target
		if !filepath.IsAbs(spelling) {
			spelling = filepath.Join(filepath.Dir(next), spelling)
		}
		if len(pending) > 0 {
			spelling = filepath.Join(append([]string{spelling}, pending...)...)
		}
		addGrant(spelling)
		if filepath.IsAbs(target) {
			cur = string(filepath.Separator)
		}
		// Do not Clean target: a/../b must resolve a's symlink before ..
		// is applied, just as the kernel does.
		pending = append(strings.Split(target, string(filepath.Separator)), pending...)
	}
	addGrant(cur)
	return cur, dirs, hops, nil
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
			if pathWithin(d, r) {
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
func resolveInterpreterChain(interp string) (dirs, hops []string, err error) {
	resolved, err := exec.LookPath(interp)
	broadFallback := false
	if err != nil {
		if filepath.IsAbs(interp) {
			resolved = interp
			broadFallback = isBroadGrant(filepath.Dir(interp))
		} else {
			return nil, nil, nil
		}
	}
	if abs, aerr := filepath.Abs(resolved); aerr == nil {
		resolved = abs
	}
	// Chain-walked for the same reason the harness itself is: an interpreter
	// reached through a version manager (nvm, mise, asdf) is a multi-hop
	// symlink, and a middle hop left ungranted fails the exec of every script
	// that names it — see walkSymlinkChain.
	_, dirs, hops, err = walkSymlinkPath(resolved, false)
	if broadFallback {
		dirs = nil
	}
	return dirs, hops, err
}

// resolveInterpreterDirs is resolveInterpreterChain's directory list alone,
// for callers that only inspect the grant surface (diagnostics, tests) and
// have no policy to check the visited paths against.
func resolveInterpreterDirs(interp string) []string {
	dirs, _, _ := resolveInterpreterChain(interp)
	return withPrefixSupportDirs(dirs)
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
	var out []string
	for _, p := range prefixSupportCandidates(binDir) {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

func prefixSupportCandidates(binDir string) []string {
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
		out = append(out, p)
	}
	return out
}

// resolvePrefixSupportDirs keeps the provenance of support-directory aliases.
// Lstat, unlike Stat, also exposes dangling or cyclic final links so an
// unfinished resolution cannot silently disappear from the policy check.
func resolvePrefixSupportDirs(dirs []string) ([]string, []string, error) {
	out := append([]string(nil), dirs...)
	var hops []string
	for _, d := range dirs {
		for _, p := range prefixSupportCandidates(d) {
			if _, err := os.Lstat(p); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return nil, nil, fmt.Errorf("inspect support directory %q: %w", p, err)
			}
			real, _, visited, err := walkSymlinkPath(p, true)
			if err != nil {
				return nil, nil, err
			}
			hops = append(hops, visited...)
			fi, err := os.Stat(real)
			if err != nil {
				return nil, nil, err
			}
			if fi.IsDir() && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out, hops, nil
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
