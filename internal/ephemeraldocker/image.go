package ephemeraldocker

import (
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ErrImageDigestMismatch reports that the base image does not match its
// pinned sha512 digest; the VM refuses to boot on it (fail closed).
var ErrImageDigestMismatch = errors.New("base image does not match the pinned digest")

const (
	imageDownloadTimeout = 10 * time.Minute
	imageFileMode        = 0o600
)

// EnsureImage makes sure the digest-pinned Alpine cloud image for arch is
// present under the cache scope and matches its pin. The image is shared
// by all sessions of the scope; a fresh private LIMA_HOME would otherwise
// re-download it on every session. Cached files are re-verified on every
// start — a corrupt cache must fail closed, not boot.
//
// urlOverride replaces the pinned download URL (tests only); the digest
// always comes from the pinned AlpineImage table, never from the wire.
func EnsureImage(cacheDir, arch, urlOverride string) (ImageSpec, error) {
	url, digest, err := AlpineImage(arch)
	if err != nil {
		return ImageSpec{}, err
	}
	if urlOverride != "" {
		url = urlOverride
	}
	return ensureImage(cacheDir, url, digest)
}

func ensureImage(cacheDir, url, digest string) (ImageSpec, error) {
	imageFile := filepath.Join(cacheDir, imageFileDir, imageFilePat)
	if err := os.MkdirAll(filepath.Dir(imageFile), 0o700); err != nil {
		return ImageSpec{}, fmt.Errorf("ephemeral-docker: image dir: %w", err)
	}

	if ok, err := verifySHA512(imageFile, digest); err == nil && ok {
		return ImageSpec{Path: imageFile, Digest: digest}, nil
	} else if err == nil {
		// Present but corrupt/stale: drop it so the download replaces it.
		if rmErr := os.Remove(imageFile); rmErr != nil && !os.IsNotExist(rmErr) {
			return ImageSpec{}, fmt.Errorf("ephemeral-docker: remove stale image: %w", rmErr)
		}
	}

	part := imageFile + ".part"
	if err := download(url, part, digest); err != nil {
		_ = os.Remove(part)
		return ImageSpec{}, err
	}
	if err := os.Rename(part, imageFile); err != nil {
		_ = os.Remove(part)
		return ImageSpec{}, fmt.Errorf("ephemeral-docker: install image: %w", err)
	}
	return ImageSpec{Path: imageFile, Digest: digest}, nil
}

func download(url, dest, digest string) error {
	client := &http.Client{Timeout: imageDownloadTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("ephemeral-docker: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ephemeral-docker: download %s: status %s", url, resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, imageFileMode)
	if err != nil {
		return fmt.Errorf("ephemeral-docker: image temp file: %w", err)
	}
	h := sha512.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		_ = f.Close()
		return fmt.Errorf("ephemeral-docker: download %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("ephemeral-docker: close image temp file: %w", err)
	}
	if got := "sha512:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("ephemeral-docker: image from %s: %w (want %s, got %s)", url, ErrImageDigestMismatch, digest, got)
	}
	return nil
}

// verifySHA512 reports whether the file at path matches the expected
// "sha512:<hex>" digest. A missing file is (false, nil); other stat/open
// errors are returned as-is.
func verifySHA512(path, expected string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	got := "sha512:" + hex.EncodeToString(h.Sum(nil))
	return got == expected, nil
}
