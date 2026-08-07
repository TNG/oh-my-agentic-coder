// Package cli build_proxy_helpers.go was the home of upstreamHost, used by
// the now-removed registryUpstreamHosts filtered-proxy allowlisting of
// private-registry upstreams. That allowlisting was a bypass path
// (spec.md:174) and was removed in the ticket-06 review: private
// registries route through the credential-lift proxy only, never the
// filtered proxy. The helper is retained as an empty placeholder so the
// file's removal does not drop a tracked path mid-change; it holds no
// symbols.
package cli
