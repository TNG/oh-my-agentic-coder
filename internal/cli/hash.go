// Package cli diagnose hash: `omac diagnose --hash[=<kind>]` prints the
// identifiers omac derives from a workdir — the start/serve runtime-dir
// hashes, the keychain secret-scope id, and the tool-cache scope digest.
//
// Everything here READS existing producers; it never recomputes a digest.
// Each kind calls the same function the runtime calls (runtimeDirPath /
// serveRuntimeDirPath in runtimedir.go, keychain.WorkdirID,
// describeCacheScope → toolcache.Describe*), so a printed hash cannot drift
// from the one omac actually uses. See issue #156.
//
// Split from diagnose.go for the same reason diagnose_probe.go is: this is a
// standalone compute-and-exit path, independent of the audit-trail analysis.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
)

// Hash kinds. "all" is the default when --hash is passed bare.
const (
	hashKindRuntime  = "runtime"
	hashKindServe    = "serve"
	hashKindKeychain = "keychain"
	hashKindCache    = "cache"
	hashKindAll      = "all"
)

// hashKinds is the emission order for --hash=all, and the list shown when an
// unknown kind is rejected.
var hashKinds = []string{hashKindRuntime, hashKindServe, hashKindKeychain, hashKindCache}

// hashEntry is one derivation. Input is the exact string that was hashed,
// which is what makes the canonicalization differences visible: runtime,
// serve and keychain hash the ABSOLUTE workdir, while cache hashes the
// EvalSymlinks'd path behind a "v1:<domain>:" prefix — on macOS, where
// /tmp is a symlink to /private/tmp, those are different strings.
type hashEntry struct {
	Kind   string `json:"kind"`
	Input  string `json:"input"`
	Digest string `json:"digest"`
	// Path is the directory the digest names, when it names one. The
	// keychain id is a namespace, not a path, so it has none.
	Path string `json:"path,omitempty"`
	// Note carries kind-specific context, e.g. the resolved cache scope.
	Note string `json:"note,omitempty"`
	// Error is set instead of Digest when this kind could not be derived.
	// In --hash=all mode one failing kind never suppresses the others.
	Error string `json:"error,omitempty"`
}

// hashView is the --json payload. The envelope is emitted even for a single
// kind so the shape is stable for scripts (`jq -r '.entries[0].path'`).
//
// TmpDir is reported because it is a real footgun: the runtime and serve
// paths are relative to os.TempDir(), so a shell whose TMPDIR differs from
// the one the running agent had resolves a different — equally correct —
// path. Seeing the TMPDIR that produced the output makes that mismatch
// diagnosable instead of mysterious.
type hashView struct {
	Workdir string      `json:"workdir"`
	TmpDir  string      `json:"tmpdir"`
	Entries []hashEntry `json:"entries"`
}

// hashKindFlag backs --hash. It is a flag.Value with IsBoolFlag so the
// stdlib accepts both the bare form (--hash) and the attached form
// (--hash=runtime); a plain string flag would reject the former.
type hashKindFlag struct {
	set  bool
	kind string
}

func (h *hashKindFlag) String() string {
	if h == nil {
		return ""
	}
	return h.kind
}

func (h *hashKindFlag) Set(v string) error {
	switch v {
	case "false":
		// --hash=false: an explicit opt-out, treated as absent.
		h.set, h.kind = false, ""
	case "", "true":
		// Bare --hash arrives here as "true".
		h.set, h.kind = true, hashKindAll
	default:
		h.set, h.kind = true, v
	}
	return nil
}

// IsBoolFlag lets `--hash` stand alone. Its consequence is that
// `--hash runtime` (space-separated) parses as bare --hash followed by a
// positional; runDiagnose rejects that explicitly rather than silently
// printing every kind.
func (h *hashKindFlag) IsBoolFlag() bool { return true }

// validHashKind reports whether kind is one this command can emit.
func validHashKind(kind string) bool {
	if kind == hashKindAll {
		return true
	}
	for _, k := range hashKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// runDiagnoseHash builds and renders the requested derivations. It reads
// nothing but the workdir, the launcher config (for the cache scope) and
// $TMPDIR — no audit trail, no sandbox profile — so it works in a fresh
// checkout and cannot be broken by an unresolvable profile.
func runDiagnoseHash(env *Env, kind string, asJSON bool) int {
	view := buildHashView(env.Workdir, kind)

	// A single explicitly requested kind that failed is a hard error: the
	// user asked for one value and there is none to print. In "all" mode the
	// failure is reported inline and the other kinds still print.
	if kind != hashKindAll && len(view.Entries) == 1 && view.Entries[0].Error != "" {
		fmt.Fprintf(env.Stderr, "omac diagnose --hash=%s: %s\n", kind, view.Entries[0].Error)
		return ExitIOError
	}

	if asJSON {
		return writeDiagnoseJSON(env, view)
	}
	return writeHashText(env, view)
}

// buildHashView derives the requested kinds. Every entry comes from the
// producer the runtime itself uses; nothing here hashes anything.
func buildHashView(workdir, kind string) hashView {
	view := hashView{Workdir: workdir, TmpDir: os.TempDir()}
	for _, k := range hashKinds {
		if kind != hashKindAll && kind != k {
			continue
		}
		view.Entries = append(view.Entries, buildHashEntry(workdir, k))
	}
	return view
}

func buildHashEntry(workdir, kind string) hashEntry {
	switch kind {
	case hashKindRuntime:
		return hashEntry{
			Kind:   kind,
			Input:  workdir,
			Digest: runtimeDirDigest(workdir),
			Path:   runtimeDirPath(workdir),
			Note:   "omac start runtime dir (logs, pids, bridge.sock)",
		}
	case hashKindServe:
		return hashEntry{
			Kind:   kind,
			Input:  "serve:" + workdir,
			Digest: serveRuntimeDirDigest(workdir),
			Path:   serveRuntimeDirPath(workdir),
			Note:   "omac serve runtime dir, keyed on the server root",
		}
	case hashKindKeychain:
		return hashEntry{
			Kind:   kind,
			Input:  workdir,
			Digest: keychain.WorkdirID(workdir),
			Note:   "secret scope id (not a path)",
		}
	case hashKindCache:
		return buildCacheHashEntry(workdir)
	}
	return hashEntry{Kind: kind, Error: "unknown kind"}
}

// buildCacheHashEntry resolves the cache scope the way the launcher does —
// load the config, resolve global/config/workdir, then hand it to the shared
// describeCacheScope (provenance.go) — so it reports the cache this workdir
// would actually get, matching `omac provenance`'s cache.path exactly.
func buildCacheHashEntry(workdir string) hashEntry {
	entry := hashEntry{Kind: hashKindCache, Input: workdir}
	lc, cfgPath, err := config.LoadLauncher(workdir)
	if err != nil {
		entry.Error = err.Error()
		return entry
	}
	cacheScope, err := lc.Cache.Resolve()
	if err != nil {
		entry.Error = err.Error()
		return entry
	}
	scope, err := describeCacheScope(cacheScope, workdir, cfgPath)
	if err != nil {
		// Most likely EvalSymlinks on a workdir that does not exist, or an
		// unset HOME. Say which scope was being resolved — the bare error
		// ("no such file or directory") is otherwise unattributable.
		entry.Error = fmt.Sprintf("resolve %s cache scope: %v", cacheScope, err)
		return entry
	}
	entry.Input = scope.Identity
	entry.Digest = scope.Digest
	entry.Path = scope.Dir
	entry.Note = fmt.Sprintf("cache scope %q (config: %s)", cacheScope, cacheScopeOrigin(cfgPath))
	return entry
}

func cacheScopeOrigin(cfgPath string) string {
	if cfgPath == "" {
		return "no config file; defaults"
	}
	return cfgPath
}

// writeHashText renders a stanza per kind. Deliberately not a table: the
// keychain and cache digests are full 64-hex sha256s, so a KIND/DIGEST/PATH
// table pads every row out past 140 columns and wraps in a normal terminal.
// Stanzas stay readable at any width and keep each digest on its own line,
// where it can be double-clicked or copied whole.
func writeHashText(env *Env, view hashView) int {
	fmt.Fprintf(env.Stdout, "workdir  %s\nTMPDIR   %s\n", view.Workdir, view.TmpDir)
	for _, e := range view.Entries {
		fmt.Fprintln(env.Stdout)
		if e.Error != "" {
			fmt.Fprintf(env.Stdout, "%s\n  error  %s\n", e.Kind, e.Error)
			continue
		}
		fmt.Fprintf(env.Stdout, "%s\n  digest %s\n", e.Kind, e.Digest)
		if e.Path != "" {
			fmt.Fprintf(env.Stdout, "  path   %s\n", e.Path)
		}
		fmt.Fprintf(env.Stdout, "  input  %s\n", e.Input)
		if e.Note != "" {
			fmt.Fprintf(env.Stdout, "  note   %s\n", e.Note)
		}
	}
	return ExitOK
}

// hashKindList renders the accepted kinds for error messages and usage.
func hashKindList() string {
	return strings.Join(append(append([]string{}, hashKinds...), hashKindAll), "|")
}
