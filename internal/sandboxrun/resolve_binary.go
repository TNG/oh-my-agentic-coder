package sandboxrun

import (
	"os"
	"os/exec"
	"path/filepath"
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
// (no env-wrapper handling): PATH-entry dir, symlink-resolved real dir, and
// shebang interpreter dirs. Returns nil when argv is empty or unresolvable.
func resolveCommandBinaryDirs(argv []string) []string {
	if len(argv) == 0 || argv[0] == "" {
		return nil
	}
	resolved, err := exec.LookPath(argv[0])
	if err != nil {
		return nil
	}
	if abs, aerr := filepath.Abs(resolved); aerr == nil {
		resolved = abs
	}
	dirs := []string{filepath.Dir(resolved)}
	if real, rerr := filepath.EvalSymlinks(resolved); rerr == nil {
		if d := filepath.Dir(real); d != dirs[0] {
			dirs = append(dirs, d)
		}
		if interp := shebangInterpreter(real); interp != "" {
			if idirs := resolveInterpreterDirs(interp); len(idirs) > 0 {
				dirs = append(dirs, idirs...)
			}
		}
	}
	return dirs
}

// unwrapEnv strips a leading `env NAME=VALUE ...` wrapper and returns the
// argv that begins with the real command. It skips `env` and any following
// NAME=VALUE assignment tokens; the first non-assignment token is the
// command. `env` flags (e.g. -i, -u) are not used by omac's profiles and are
// left in place, so an argv like `env -i cmd` yields `-i cmd` — which fails
// to resolve and is safely ignored by the caller. Returns argv unchanged when
// there is no env wrapper.
func unwrapEnv(argv []string) []string {
	for len(argv) >= 2 && filepath.Base(argv[0]) == "env" {
		i := 1
		for i < len(argv) && isEnvAssignment(argv[i]) {
			i++
		}
		argv = argv[i:]
	}
	return argv
}

// isEnvAssignment reports whether tok is a NAME=VALUE assignment (not a flag).
func isEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	return eq > 0 && !strings.HasPrefix(tok, "-")
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
				return []string{filepath.Dir(interp)}
			}
		}
		return nil
	}
	if abs, aerr := filepath.Abs(resolved); aerr == nil {
		resolved = abs
	}
	dirs := []string{filepath.Dir(resolved)}
	if real, rerr := filepath.EvalSymlinks(resolved); rerr == nil {
		if d := filepath.Dir(real); d != dirs[0] {
			dirs = append(dirs, d)
		}
	}
	return dirs
}
