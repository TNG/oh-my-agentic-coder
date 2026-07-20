package netproxy

import (
	"context"
	"net"
	"net/netip"
)

// ClientSource identifies the sandboxed connection the proxy is handling: the
// child's socket endpoint (Src) and the proxy's own endpoint (Proxy). It is
// carried on the request context purely so the prompt can attribute the dial
// to a process — the filter's admission logic never reads it.
type ClientSource struct {
	Src   netip.AddrPort
	Proxy netip.AddrPort
}

type clientSourceKey struct{}

// WithClientSource returns ctx annotated with the connection's endpoints.
func WithClientSource(ctx context.Context, src, proxy netip.AddrPort) context.Context {
	return context.WithValue(ctx, clientSourceKey{}, ClientSource{Src: src, Proxy: proxy})
}

// ClientSourceFromContext recovers endpoints stored by WithClientSource.
func ClientSourceFromContext(ctx context.Context) (ClientSource, bool) {
	cs, ok := ctx.Value(clientSourceKey{}).(ClientSource)
	return cs, ok
}

// addrPort converts a net.Addr (as returned by conn.RemoteAddr/LocalAddr) to a
// netip.AddrPort, returning the zero value when it is not a TCP address.
func addrPort(a net.Addr) netip.AddrPort {
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.AddrPort()
	}
	ap, _ := netip.ParseAddrPort(a.String())
	return ap
}

// clientSourceCtx returns a background context annotated with conn's endpoints,
// so the prompt (reached deep inside the filter) can attribute the dial.
func clientSourceCtx(conn net.Conn) context.Context {
	return WithClientSource(context.Background(), addrPort(conn.RemoteAddr()), addrPort(conn.LocalAddr()))
}
