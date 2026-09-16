package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// Runtime-dir name derivation, kept separate from the functions that create
// those directories.
//
// createRuntimeDir (start.go) and createRuntimeDirServe (serve.go) both
// os.RemoveAll their target before recreating it, so nothing that merely wants
// to KNOW the path may call them — `omac diagnose --hash` would wipe a running
// session's logs/ and pids/. The pure derivations therefore live here, and the
// creating functions call them. That is the single source of truth: there is
// exactly one sha256 of a workdir in the CLI package, and both the runtime and
// the reporting path go through it, so the printed hash cannot drift from the
// one omac actually uses.
//
// Note the digest is over the ABSOLUTE workdir with no symlink resolution
// (cli.Run absolutizes but does not EvalSymlinks) — unlike the tool cache,
// which hashes the EvalSymlinks'd path. See internal/toolcache/cache.go.

// shortDigest returns the first 6 bytes of sha256(input) as hex — the
// truncation both runtime-dir names use.
func shortDigest(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:6])
}

// runtimeDirDigest is the hash embedded in a start-mode runtime dir name.
func runtimeDirDigest(workdir string) string { return shortDigest(workdir) }

// runtimeDirPath returns ${TMPDIR}/omac-<digest> for a workdir, without
// creating, touching, or removing anything.
func runtimeDirPath(workdir string) string {
	return filepath.Join(os.TempDir(), "omac-"+runtimeDirDigest(workdir))
}

// serveRuntimeDirDigest is the hash embedded in a serve-mode runtime dir name.
// The "serve:" prefix is what keeps a serve run's dir distinct from a start
// run's dir for the same directory.
func serveRuntimeDirDigest(serverRoot string) string { return shortDigest("serve:" + serverRoot) }

// serveRuntimeDirPath returns ${TMPDIR}/omac-serve-<digest> for a server root,
// without creating, touching, or removing anything.
func serveRuntimeDirPath(serverRoot string) string {
	return filepath.Join(os.TempDir(), "omac-serve-"+serveRuntimeDirDigest(serverRoot))
}
