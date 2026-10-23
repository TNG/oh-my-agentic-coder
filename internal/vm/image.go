package vm

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// ImagePuller downloads the image ref into a docker-save style tarball at
// tarPath. CranePull is the production implementation; tests substitute fakes.
type ImagePuller func(ctx context.Context, ref, tarPath string) error

// CranePull pulls ref from its registry without a Docker daemon. It uses
// crane's default HTTP client, so the host's proxy settings apply, and the
// host keychain (~/.docker/config.json, credential helpers) for auth.
//
// The tarball is written to tarPath+".part" and renamed on success, so
// tarPath never holds a partial image.
//
// Multi-arch images resolve to linux/runtime.GOARCH because the Lima guest
// runs with the host's architecture; crane would otherwise pick linux/amd64.
func CranePull(ctx context.Context, ref, tarPath string) error {
	img, err := crane.Pull(ref,
		crane.WithContext(ctx),
		crane.WithAuthFromKeychain(authn.DefaultKeychain),
		crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: runtime.GOARCH}),
	)
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	part := tarPath + ".part"
	if err := crane.Save(img, ref, part); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("save %s: %w", ref, err)
	}
	if err := os.Rename(part, tarPath); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("save %s: %w", ref, err)
	}
	return nil
}

// ValidateImageRef parses ref and returns its fully qualified form, for
// example "alpine" becomes "index.docker.io/library/alpine:latest". Digest
// references are rejected: docker load cannot name an image by digest.
func ValidateImageRef(ref string) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("invalid image reference %q: %w", ref, err)
	}
	if _, ok := parsed.(name.Digest); ok {
		return "", fmt.Errorf("image reference %q pins a digest, which docker load cannot keep; use a tag", ref)
	}
	return parsed.Name(), nil
}
