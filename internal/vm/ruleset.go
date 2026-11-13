package vm

import _ "embed"

//go:embed omac-vmguard.nft
var vmguardRuleset string

// Ruleset returns the embedded omac-vmguard nftables ruleset that the boot
// provisioning script writes to RulesetFile and loads in offline mode.
func Ruleset() string {
	return vmguardRuleset
}
