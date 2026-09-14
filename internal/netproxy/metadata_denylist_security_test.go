//go:build vuln

package netproxy

import (
	"context"
	"testing"
)

// Cloud instance-metadata coverage.
//
// A cloud VM's metadata service hands out the instance's credentials to any
// process that can reach it, with no authentication beyond being on the box.
// That makes it the single highest-value destination a confined agent can
// aim at, which is why omac hard-denies it: not merely blocked, but never
// offered to the user as a prompt, because "allow" is never a safe answer.
//
// The guarantee is only as wide as the address list behind it. An endpoint
// that is missing from the list is not partially protected — it is wide open
// in blocklist mode, where anything unmatched is allowed by default.

// TestSecurityCloudMetadataEndpointsAlwaysDenied asserts that every metadata
// address omac may plausibly run next to is hard-denied and unpromptable.
//
// The list covers the three major providers' documented endpoints. Two gaps
// matter in particular: Alibaba Cloud answers on 100.100.100.200, which is
// ordinary unicast space and matches no hard-deny rule; and AWS serves the
// same credential API over IPv6 at fd00:ec2::254, a unique-local address
// that the link-local check does not cover, so an agent that simply prefers
// IPv6 walks around an IPv4-only denial.
func TestSecurityCloudMetadataEndpointsAlwaysDenied(t *testing.T) {
	endpoints := []struct {
		host string
		what string
	}{
		{"169.254.169.254", "AWS/GCP/Azure IMDS over IPv4"},
		{"metadata.google.internal", "GCP metadata hostname"},
		{"metadata.azure.internal", "Azure metadata hostname"},
		{"fd00:ec2::254", "AWS IMDS over IPv6"},
		{"100.100.100.200", "Alibaba Cloud metadata"},
	}

	for _, e := range endpoints {
		// A fresh filter per endpoint so the prompt count below is
		// attributable to exactly one host.
		p := &fakePrompter{res: PromptResult{Allow: true}}
		f := NewFilter(FilterConfig{
			PromptEnabled: true,
			Prompter:      p,
			// Never consulted for an IP literal; set so a hostname entry
			// cannot fall out on a DNS error and look denied for the wrong
			// reason.
			Resolve: staticResolver("93.184.216.34"),
		})

		v, _ := f.Check(context.Background(), e.host, 80)
		if v.Decision != Deny {
			t.Errorf("Check(%s) = Allow: %s is reachable from the sandbox, and it serves instance credentials to any caller", e.host, e.what)
			continue
		}
		if p.calls != 0 {
			t.Errorf("Check(%s) prompted the user: %s must be denied outright, never offered as a choice", e.host, e.what)
		}
	}
}
