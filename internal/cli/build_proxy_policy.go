package cli

// publicGradleMavenAllowlist is the set of public Gradle/Maven endpoints
// the build path's filtered proxy (internal/netproxy) allows for PUBLIC
// dependency resolution (criterion 6). These go through the existing
// filtered proxy as direct CONNECT tunnels — NO TLS interception
// (spec.md:57, 180). Only the declared private registries route through
// the credential-lift proxy (internal/credproxy).
//
// The list is the operational default per spec.md:176 ("Public Gradle
// and Maven endpoints needed by standard builds may be detected as
// operational defaults"). It covers:
//   - Maven Central + Sonatype (dependency + metadata)
//   - Gradle plugin/distribution/services hosts
//   - JitPack (common Gradle plugin source)
//   - common Maven mirrors a standard Gradle build hits
//
// Wildcards (*.host) match the host and any subdomain. A non-wildcard
// entry matches the exact host only. Matching is case-insensitive
// (netproxy.MatchDomainList).
var publicGradleMavenAllowlist = []string{
	// Maven Central + Sonatype.
	"repo.maven.apache.org",
	"repo1.maven.org",
	"central.maven.org",
	"search.maven.org",
	"oss.sonatype.org",
	"s01.oss.sonatype.org",
	"repo.maven.sonatype.com",
	// Gradle distribution / plugin / services hosts.
	"services.gradle.org",
	"downloads.gradle.org",
	"plugins.gradle.org",
	"repo.gradle.org",
	"gradle.org",
	// JitPack (common Gradle plugin source).
	"jitpack.io",
	// Common public Maven mirrors.
	"repo.spring.io",
	"maven.springframework.org",
	"repository.apache.org",
	"repository.jboss.org",
	"maven.aliyun.com",
}

// buildScanDenylist is the set of hosts a Gradle build scan would upload
// to. The spec (non-goals, spec.md:56) forbids Gradle build scans unless
// separately and explicitly allowed after proxy-log leakage is
// eliminated; ticket 06 denies the scan upload hosts so a `--scan`
// attempt is blocked at the filtered proxy (criterion 4).
var buildScanDenylist = []string{
	"scans.gradle.com",
	"ge.gradle.org",
	"scan.gradle.com",
}
