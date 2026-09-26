// Package main verifies a release checksums.txt signature against the pinned
// ed25519 public key. Usage:
//
//	go run ./scripts/verify-checksums checksums.txt checksums.txt.sig
//
// The public key below must match pinnedReleaseSigningKey in
// internal/updater/updater.go. A test in internal/updater checks this
// consistency.
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
)

// pinnedKey must match internal/updater.pinnedReleaseSigningKey.
const pinnedKey = "866775807484337b8447c737fecb572f26ee6905b803ab45ba8bfb07bc85e2a0"

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: verify-checksums <checksums.txt> <checksums.txt.sig>")
		os.Exit(2)
	}

	keyBytes, err := hex.DecodeString(pinnedKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid pinned key hex: %v\n", err)
		os.Exit(1)
	}
	if len(keyBytes) != ed25519.PublicKeySize {
		fmt.Fprintf(os.Stderr, "pinned key must be %d bytes, got %d\n", ed25519.PublicKeySize, len(keyBytes))
		os.Exit(1)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
	sig, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", os.Args[2], err)
		os.Exit(1)
	}

	if !ed25519.Verify(ed25519.PublicKey(keyBytes), data, sig) {
		fmt.Fprintln(os.Stderr, "signature verification failed")
		os.Exit(1)
	}
	fmt.Println("signature verified")
}
