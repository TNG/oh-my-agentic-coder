// Package ephemeraldocker implements the --ephemeral-docker capability:
// a per-session throwaway Lima VM with its own Docker daemon, managed by
// the unsandboxed omac parent during the runLaunch lifecycle
// (openspec/changes/add-ephemeral-docker).
//
// The VM boundary is defined by the generated Lima config (mounts: [],
// own disk, no host sockets or credentials) and the embedded
// omac-vmguard nftables ruleset that separates running containers from
// host gateway, host LAN, and the internet.
package ephemeraldocker

import _ "embed"

// vmguardRuleset holds the embedded omac-vmguard nftables ruleset, the
// production copy of the spike-measured file.
//
//go:embed omac-vmguard.nft
var vmguardRuleset string

// Ruleset returns the embedded omac-vmguard nftables ruleset that the
// Lima provision step installs into the session VM.
func Ruleset() string {
	return vmguardRuleset
}
