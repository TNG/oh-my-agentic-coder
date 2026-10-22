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
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
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
	// Keep authn.DefaultKeychain away from the developer's real Docker config
	// and credential helpers.
	t.Setenv("DOCKER_CONFIG", t.TempDir())
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

// The guest VM has the host's architecture, so a multi-arch image must resolve
// to runtime.GOARCH rather than go-containerregistry's linux/amd64 default.
func TestCranePullPicksHostArchitecture(t *testing.T) {
	host := startTestRegistry(t)
	ref := host + "/test/multi:v1"
	other := "amd64"
	if runtime.GOARCH == "amd64" {
		other = "arm64"
	}
	idx := v1.ImageIndex(empty.Index)
	for _, arch := range []string{other, runtime.GOARCH} {
		img, err := random.Image(256, 1)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := img.ConfigFile()
		if err != nil {
			t.Fatal(err)
		}
		cfg = cfg.DeepCopy()
		cfg.OS, cfg.Architecture = "linux", arch
		if img, err = mutate.ConfigFile(img, cfg); err != nil {
			t.Fatal(err)
		}
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}},
		})
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(parsed, idx); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	tarPath := filepath.Join(t.TempDir(), "img.tar")
	if err := CranePull(context.Background(), ref, tarPath); err != nil {
		t.Fatalf("CranePull: %v", err)
	}
	got, err := tarball.ImageFromPath(tarPath, nil)
	if err != nil {
		t.Fatalf("read tarball: %v", err)
	}
	cfg, err := got.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Architecture != runtime.GOARCH {
		t.Errorf("pulled architecture = %q, want %q", cfg.Architecture, runtime.GOARCH)
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
