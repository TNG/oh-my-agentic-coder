package ephemeraldocker

import (
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func sha512Hex(b []byte) string {
	h := sha512.Sum512(b)
	return hex.EncodeToString(h[:])
}

func TestEnsureImageDownloadsAndVerifies(t *testing.T) {
	body := []byte("fake-qcow2-image-bytes")
	// The digest is pinned ahead of the download (production pins the
	// measured Alpine digest); the server bytes must match it.
	digest := "sha512:" + sha512Hex(body)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	root := t.TempDir()
	spec, err := ensureImage(root, srv.URL+"/alpine.qcow2", digest)
	if err != nil {
		t.Fatalf("ensureImage: %v", err)
	}
	if spec.Digest != digest {
		t.Errorf("digest mismatch: %s", spec.Digest)
	}
	if got, _ := os.ReadFile(spec.Path); string(got) != string(body) {
		t.Error("downloaded file content differs")
	}
	if hits.Load() != 1 {
		t.Errorf("expected exactly one download, got %d", hits.Load())
	}

	// Second call must reuse the cached file without a download.
	spec2, err := ensureImage(root, srv.URL+"/alpine.qcow2", digest)
	if err != nil {
		t.Fatalf("ensureImage (cached): %v", err)
	}
	if spec2.Path != spec.Path || hits.Load() != 1 {
		t.Errorf("cache reuse failed (path=%s hits=%d)", spec2.Path, hits.Load())
	}
}

func TestEnsureImageRejectsDigestMismatch(t *testing.T) {
	honest := []byte("honest-image")
	tampered := []byte("tampered-image")
	var flip atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if flip.Swap(true) {
			_, _ = w.Write(tampered)
			return
		}
		_, _ = w.Write(honest)
	}))
	defer srv.Close()

	root := t.TempDir()
	url := srv.URL + "/alpine.qcow2"
	digest := "sha512:" + sha512Hex(honest)
	if _, err := ensureImage(root, url, digest); err != nil {
		t.Fatalf("first ensureImage: %v", err)
	}
	// Cache holds the honest image; tamper with it to prove the cached
	// file is re-verified, not blindly trusted.
	img := filepath.Join(root, "ephemeral-docker", "alpine-cloud.qcow2")
	if err := os.WriteFile(img, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	// The next server read must serve tampered bytes so both layers are
	// proven: the cached file is re-verified (removal) and the wire bytes
	// are rejected (ErrImageDigestMismatch).
	flip.Store(true)
	_, err := ensureImage(root, url, digest)
	if err == nil {
		t.Fatal("a corrupted cached image must be rejected")
	}
	if !errors.Is(err, ErrImageDigestMismatch) {
		t.Errorf("expected ErrImageDigestMismatch, got %v", err)
	}
	// The corrupt file must be removed so the next start re-downloads.
	if _, statErr := os.Stat(img); statErr == nil {
		t.Error("corrupted cached image must be removed after a digest mismatch")
	}
}

func TestEnsureImageUnpinnedArchFails(t *testing.T) {
	root := t.TempDir()
	_, err := EnsureImage(root, "x86_64", "https://example.invalid/x.qcow2")
	if !errors.Is(err, ErrUnpinnedArch) {
		t.Errorf("x86_64 must fail closed with ErrUnpinnedArch, got %v", err)
	}
}

func TestFindLimactl(t *testing.T) {
	if _, err := FindLimactl(func(string) (string, error) { return "/opt/homebrew/bin/limactl", nil }); err != nil {
		t.Errorf("existing limactl must resolve, got %v", err)
	}
	_, err := FindLimactl(func(string) (string, error) { return "", os.ErrNotExist })
	if !errors.Is(err, ErrLimaMissing) {
		t.Errorf("missing limactl must surface ErrLimaMissing, got %v", err)
	}
}
