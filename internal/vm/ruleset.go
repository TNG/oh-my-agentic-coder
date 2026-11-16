package vm

import (
	_ "embed"
	"strings"
)

//go:embed omac-vmguard.nft
var vmguardRuleset string

// Ruleset returns the embedded omac-vmguard nftables ruleset that the boot
// provisioning script writes to RulesetFile and loads in offline mode. A
// Windows checkout embeds the file with CRLF; the guest needs LF.
func Ruleset() string {
	return strings.ReplaceAll(vmguardRuleset, "\r\n", "\n")
}
