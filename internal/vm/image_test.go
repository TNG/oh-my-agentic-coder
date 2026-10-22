package vm

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

func TestValidateImageRef(t *testing.T) {
	cases := []struct {
		ref  string
		want string
	}{
		{"alpine", "index.docker.io/library/alpine:latest"},
		{"pgvector/pgvector:pg16", "index.docker.io/pgvector/pgvector:pg16"},
	}
	for _, c := range cases {
		got, err := ValidateImageRef(c.ref)
		if err != nil {
			t.Fatalf("ValidateImageRef(%q): %v", c.ref, err)
		}
		if got != c.want {
			t.Errorf("ValidateImageRef(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
	if _, err := ValidateImageRef("not a ref!"); err == nil {
		t.Error(`ValidateImageRef("not a ref!") succeeded, want error`)
	}
}

// startTestRegistry serves an in-memory registry on loopback and returns its
// host:port. go-containerregistry talks plain HTTP to 127.0.0.1 by default.
func startTestRegistry(t *testing.T) string {
	t.Helper()
	sectest.RequireLoopbackListener(t)
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestCranePullWritesTarball(t *testing.T) {
	host := startTestRegistry(t)
	ref := host + "/test/img:v1"
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(parsed, img); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	want, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}

	tarPath := filepath.Join(t.TempDir(), "img.tar")
	if err := CranePull(context.Background(), ref, tarPath); err != nil {
		t.Fatalf("CranePull: %v", err)
	}
	got, err := tarball.ImageFromPath(tarPath, nil)
	if err != nil {
		t.Fatalf("read tarball: %v", err)
	}
	gotDigest, err := got.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if gotDigest != want {
		t.Errorf("digest = %s, want %s", gotDigest, want)
	}
	if _, err := os.Stat(tarPath + ".part"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s.part left behind (stat err = %v)", tarPath, err)
	}
}

func TestCranePullMissingTagLeavesNoFile(t *testing.T) {
	host := startTestRegistry(t)
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "img.tar")
	if err := CranePull(context.Background(), host+"/test/missing:nope", tarPath); err == nil {
		t.Fatal("CranePull of a missing tag succeeded, want error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("files left behind after failed pull: %v", entries)
	}
}
