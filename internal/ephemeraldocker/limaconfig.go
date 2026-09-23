package ephemeraldocker

import (
	"errors"
	"fmt"
	"strings"
)

// GuestEndpointPort is the port dockerd binds inside the guest (loopback
// only). The measured daemon.json/argv pair from the firewall spike pins
// this; the host side uses a per-session deterministic port instead.
const GuestEndpointPort = 2375

// ErrUnpinnedArch reports that no measured image digest exists for the
// requested architecture. The VM refuses to boot unpinned (fail closed).
var ErrUnpinnedArch = errors.New("no pinned image digest for this architecture")

const (
	alpineRelease    = "3.22.4"
	alpineURLFormat  = "https://dl-cdn.alpinelinux.org/alpine/v3.22/releases/cloud/nocloud_alpine-%s-" + alpineRelease + "-uefi-cloudinit-r0.qcow2"
	alpineAarch64SHA = "a53620902f99b1fd4591125d348e4385f036d590607f4fa90d6f85fea0248007ec763cc81d025f347ede5bd746b3726cf5eddd461b6c94c35376f71cb7558317"
)

// AlpineImage returns the download URL and the pinned sha512 digest of the
// Alpine cloud image for arch. Digests are measured values from the spike
// (scratch SSOT); an architecture without a measurement fails closed with
// ErrUnpinnedArch rather than booting an unpinned image.
func AlpineImage(arch string) (url, digest string, err error) {
	switch arch {
	case "aarch64":
		digest = "sha512:" + alpineAarch64SHA
	default:
		return "", "", fmt.Errorf("alpine %s: %w", arch, ErrUnpinnedArch)
	}
	return fmt.Sprintf(alpineURLFormat, arch), digest, nil
}

// ImageSpec names the local image file the VM boots from and the digest it
// must match. The file is prefetched into the cache scope; Lima never sees
// the remote URL, so a session cannot silently re-download an unpinned
// image.
type ImageSpec struct {
	Path   string
	Digest string
}

// LimaConfig carries the per-session inputs of the generated Lima config.
type LimaConfig struct {
	// VMName is the limactl instance name, e.g. omac-eph-<hex>. It is the
	// marker the sweep and the QEMU reaper match on.
	VMName string
	// HostPort is the deterministic loopback port on the host that forwards
	// to the guest docker endpoint.
	HostPort int
	// Arch is the guest architecture (aarch64, x86_64).
	Arch string
	// Image is the local, digest-pinned Alpine cloud image.
	Image ImageSpec
	// Ruleset is the embedded omac-vmguard nftables file.
	Ruleset string
}

// RenderLimaConfig synthesizes the per-session Lima YAML. The boundary
// requirements are structural parts of the output (mounts: [], own disk,
// no host sockets, no host credentials, vmguard provision before dockerd),
// see openspec/changes/add-ephemeral-docker/design.md.
func RenderLimaConfig(cfg LimaConfig) (string, error) {
	if cfg.VMName == "" {
		return "", errors.New("ephemeral-docker: VMName is required")
	}
	if cfg.HostPort <= 0 || cfg.HostPort >= 65536 {
		return "", fmt.Errorf("ephemeral-docker: HostPort %d out of range", cfg.HostPort)
	}
	if cfg.Arch == "" {
		return "", errors.New("ephemeral-docker: Arch is required")
	}
	if cfg.Image.Path == "" || cfg.Image.Digest == "" {
		return "", errors.New("ephemeral-docker: image path and digest are required")
	}
	if strings.TrimSpace(cfg.Ruleset) == "" {
		return "", errors.New("ephemeral-docker: ruleset is required")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# omac ephemeral-docker: per-session throwaway Docker VM (%s).\n", cfg.VMName)
	b.WriteString("# Generated file. Session state lives under the omac cache scope.\n")
	b.WriteString("vmType: qemu\n")
	b.WriteString("cpus: 2\n")
	b.WriteString("memory: \"3GiB\"\n")
	b.WriteString("disk: \"15GiB\"\n")
	b.WriteString("images:\n")
	fmt.Fprintf(&b, "- location: %q\n", cfg.Image.Path)
	fmt.Fprintf(&b, "  arch: %q\n", cfg.Arch)
	fmt.Fprintf(&b, "  digest: %q\n", cfg.Image.Digest)
	// VM boundary: no host filesystem mounts at all. Docker bind mounts
	// see only the guest filesystem.
	b.WriteString("mounts: []\n")
	b.WriteString("containerd: {system: false, user: false}\n")
	b.WriteString("provision:\n")

	// 1. packages: docker daemon+cli and nftables for the vmguard ruleset.
	b.WriteString("- mode: system\n")
	b.WriteString("  script: |\n")
	b.WriteString("    #!/bin/sh\n")
	b.WriteString("    apk add --no-cache docker docker-cli nftables\n")

	// 2. omac-vmguard: write the embedded ruleset and load it. A failed
	// load fails the provision, so the endpoint wait never succeeds and
	// the session never starts without the firewall.
	b.WriteString("- mode: system\n")
	b.WriteString("  script: |\n")
	b.WriteString("    #!/bin/sh\n")
	b.WriteString("    cat > /etc/omac-vmguard.nft <<'OMACVMGUARD'\n")
	for _, line := range strings.Split(strings.TrimRight(cfg.Ruleset, "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	b.WriteString("    OMACVMGUARD\n")
	b.WriteString("    nft -f /etc/omac-vmguard.nft || { echo \"omac-vmguard install failed\"; exit 1; }\n")
	b.WriteString("    nft list table inet omac-vmguard\n")

	// 3. dockerd: guest-loopback TCP endpoint plus the unix socket for the
	// guest-internal probe. Published container ports bind guest loopback
	// so Lima's auto-forward exposes them on host loopback (spike probe D).
	b.WriteString("- mode: system\n")
	b.WriteString("  script: |\n")
	b.WriteString("    #!/bin/sh\n")
	b.WriteString("    [ -x /usr/bin/dockerd ] || { echo \"dockerd missing\"; exit 0; }\n")
	b.WriteString("    mkdir -p /etc/docker\n")
	b.WriteString("    echo '{\"ip\":\"127.0.0.1\",\"iptables\":true}' > /etc/docker/daemon.json\n")
	b.WriteString("    nohup dockerd -H unix:///var/run/docker.sock -H tcp://127.0.0.1:2375 > /var/log/dockerd.log 2>&1 &\n")

	// Guest-side readiness probe; the authoritative gate is the host-side
	// endpoint wait in the omac parent.
	b.WriteString("probes:\n")
	b.WriteString("- script: |\n")
	b.WriteString("    #!/bin/sh\n")
	b.WriteString("    timeout 90 sh -c 'until docker version >/dev/null 2>&1; do sleep 1; done'\n")

	// Explicit forward of the docker endpoint to the deterministic host
	// loopback port; DOCKER_HOST inside the sandbox points here.
	b.WriteString("portForwards:\n")
	b.WriteString("- guestIP: \"127.0.0.1\"\n")
	fmt.Fprintf(&b, "  guestPort: %d\n", GuestEndpointPort)
	b.WriteString("  hostIP: \"127.0.0.1\"\n")
	fmt.Fprintf(&b, "  hostPort: %d\n", cfg.HostPort)
	return b.String(), nil
}
