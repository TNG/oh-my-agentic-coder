// Package main signs checksums.txt with ed25519 for a GoReleaser release.
//
// Usage: sign-checksums <checksums-file> <signature-output>
//
// The 32-byte ed25519 seed is read from the OMAC_RELEASE_SIGNING_SEED
// environment variable (hex-encoded). The corresponding public key is
// pinned in internal/updater.updater.go (pinnedReleaseSigningKey) so the
// updater can verify authenticity without trusting a key fetched from the
// same release.
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: sign-checksums <checksums-file> <signature-output>")
		os.Exit(2)
	}

	seedHex := os.Getenv("OMAC_RELEASE_SIGNING_SEED")
	if seedHex == "" {
		fmt.Fprintln(os.Stderr, "OMAC_RELEASE_SIGNING_SEED is not set; use --skip=sign for local snapshot builds")
		os.Exit(1)
	}
	seed, err := hex.DecodeString(seedHex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid seed hex: %v\n", err)
		os.Exit(1)
	}
	if len(seed) != ed25519.SeedSize {
		fmt.Fprintf(os.Stderr, "seed must be %d bytes, got %d\n", ed25519.SeedSize, len(seed))
		os.Exit(1)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}

	priv := ed25519.NewKeyFromSeed(seed)
	sig := ed25519.Sign(priv, data)

	if err := os.WriteFile(os.Args[2], sig, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", os.Args[2], err)
		os.Exit(1)
	}
}
