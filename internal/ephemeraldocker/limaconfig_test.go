package ephemeraldocker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testLimaConfig() LimaConfig {
	return LimaConfig{
		VMName:   "omac-eph-a1b2c3d4",
		HostPort: 31234,
		Arch:     "aarch64",
		Image: ImageSpec{
			Path:   "/cache/omac/ephemeral-docker/alpine-cloud.qcow2",
			Digest: "sha512:" + strings.Repeat("ab", 64),
		},
		Ruleset: Ruleset(),
	}
}

func TestRenderLimaConfigGolden(t *testing.T) {
	for _, arch := range []string{"aarch64", "x86_64"} {
		t.Run(arch, func(t *testing.T) {
			cfg := testLimaConfig()
			cfg.Arch = arch
			got, err := RenderLimaConfig(cfg)
			if err != nil {
				t.Fatalf("RenderLimaConfig: %v", err)
			}
			golden := filepath.Join("testdata", "lima-"+arch+".golden")
			if update := os.Getenv("UPDATE_GOLDEN"); update != "" {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatalf("update golden: %v", err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if got != string(want) {
				t.Errorf("RenderLimaConfig(%s) differs from %s", arch, golden)
			}
		})
	}
}

func TestRenderLimaConfigBoundary(t *testing.T) {
	got, err := RenderLimaConfig(testLimaConfig())
	if err != nil {
		t.Fatalf("RenderLimaConfig: %v", err)
	}
	for _, want := range []string{
		"mounts: []",
		"containerd: {system: false, user: false}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q", want)
		}
	}
	if strings.Contains(got, "guestSocket") || strings.Contains(got, "hostSocket") {
		t.Error("config must not forward any guest socket to the host")
	}
	if n := strings.Count(got, "memory:"); n != 1 {
		t.Errorf("expected exactly one memory line, got %d", n)
	}
}

func TestRenderLimaConfigProvisionOrder(t *testing.T) {
	got, err := RenderLimaConfig(testLimaConfig())
	if err != nil {
		t.Fatalf("RenderLimaConfig: %v", err)
	}
	pkg := strings.Index(got, "apk add --no-cache docker docker-cli nftables")
	vmguard := strings.Index(got, "nft -f /etc/omac-vmguard.nft")
	dockerd := strings.Index(got, "nohup dockerd -H unix:///var/run/docker.sock -H tcp://127.0.0.1:2375")
	probe := strings.Index(got, "until DOCKER_HOST=tcp://127.0.0.1:2375 docker version")
	if pkg < 0 || vmguard < 0 || dockerd < 0 || probe < 0 {
		t.Fatalf("config misses a provision stage (pkg=%d vmguard=%d dockerd=%d probe=%d)",
			pkg, vmguard, dockerd, probe)
	}
	if !(pkg < vmguard && vmguard < dockerd && dockerd < probe) {
		t.Errorf("provision order wrong: pkg=%d vmguard=%d dockerd=%d probe=%d",
			pkg, vmguard, dockerd, probe)
	}
}

func TestRenderLimaConfigEmbedsRuleset(t *testing.T) {
	got, err := RenderLimaConfig(testLimaConfig())
	if err != nil {
		t.Fatalf("RenderLimaConfig: %v", err)
	}
	// The full ruleset is written into the guest before `nft -f` loads it;
	// a truncated embed would install a silently weaker firewall. The
	// heredoc indents every line by four spaces, so compare the block with
	// the indent stripped: it must be byte-identical to the ruleset.
	start := strings.Index(got, "<<'OMACVMGUARD'\n")
	end := strings.Index(got, "    OMACVMGUARD\n")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("config has no OMACVMGUARD heredoc (start=%d end=%d)", start, end)
	}
	var guest strings.Builder
	for _, line := range strings.Split(got[start+len("<<'OMACVMGUARD'\n"):end], "\n") {
		guest.WriteString(strings.TrimPrefix(line, "    ") + "\n")
	}
	if guest.String() != Ruleset()+"\n" {
		t.Errorf("heredoc content differs from the embedded ruleset:\n%s", guest.String())
	}
	if !strings.Contains(got, "omac-vmguard install failed") {
		t.Error("vmguard provision must fail the boot when nft cannot load the ruleset")
	}
}

func TestRenderLimaConfigPortForward(t *testing.T) {
	got, err := RenderLimaConfig(testLimaConfig())
	if err != nil {
		t.Fatalf("RenderLimaConfig: %v", err)
	}
	for _, want := range []string{
		"guestIP: \"127.0.0.1\"",
		"guestPort: 2375",
		"hostIP: \"127.0.0.1\"",
		"hostPort: 31234",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("port forward missing %q", want)
		}
	}
}

func TestRenderLimaConfigDeterministic(t *testing.T) {
	a, err := RenderLimaConfig(testLimaConfig())
	if err != nil {
		t.Fatalf("first render: %v", err)
	}
	b, err := RenderLimaConfig(testLimaConfig())
	if err != nil {
		t.Fatalf("second render: %v", err)
	}
	if a != b {
		t.Error("two renders of the same config must be byte-identical")
	}
}

func TestAlpineImagePinsDigest(t *testing.T) {
	url, digest, err := AlpineImage("aarch64")
	if err != nil {
		t.Fatalf("AlpineImage(aarch64): %v", err)
	}
	// The real filename on dl-cdn puts the release before the arch;
	// the exact-match check guards against placeholder-order regressions
	// that pass Contains/HasSuffix checks but 404 on download.
	if url != "https://dl-cdn.alpinelinux.org/alpine/v3.22/releases/cloud/nocloud_alpine-3.22.4-aarch64-uefi-cloudinit-r0.qcow2" {
		t.Errorf("unexpected image URL %q", url)
	}
	if !strings.HasPrefix(digest, "sha512:") || len(digest) != len("sha512:")+128 {
		t.Errorf("digest must be a sha512 hex pin, got %q", digest)
	}
	// The measured aarch64 digest from the spike (scratch SSOT).
	if digest != "sha512:a53620902f99b1fd4591125d348e4385f036d590607f4fa90d6f85fea0248007ec763cc81d025f347ede5bd746b3726cf5eddd461b6c94c35376f71cb7558317" {
		t.Errorf("aarch64 digest drifted from the measured spike digest: %q", digest)
	}
	_, _, err = AlpineImage("x86_64")
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("x86_64 has no measured digest and must fail closed, got err=%v", err)
	}
	if !errors.Is(err, ErrUnpinnedArch) {
		t.Errorf("x86_64 must fail with ErrUnpinnedArch, got %v", err)
	}
}
