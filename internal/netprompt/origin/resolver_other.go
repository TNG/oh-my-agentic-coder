//go:build !linux

package origin

import "net/netip"

func platformResolver() Resolver { return noopResolver{} }

type noopResolver struct{}

func (noopResolver) Resolve(_, _ netip.AddrPort) (Origin, bool) { return Origin{}, false }
