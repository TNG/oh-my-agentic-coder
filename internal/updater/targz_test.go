package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"testing"
)

// writeTestTarGz builds a gzip-compressed tar archive at path containing a
// single regular file entry named name with the given content and mode.
func writeTestTarGz(t *testing.T, path, name string, content []byte, mode int64) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: mode,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("write tar content: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write tar.gz file: %v", err)
	}
}

func TestExtractBinary_Found(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/asset.tar.gz"
	writeTestTarGz(t, path, "omac", []byte("binary-content"), 0o755)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	data, mode, err := ExtractBinary(f, "omac")
	if err != nil {
		t.Fatalf("ExtractBinary: %v", err)
	}
	if string(data) != "binary-content" {
		t.Fatalf("data = %q", data)
	}
	if mode != 0o755 {
		t.Fatalf("mode = %v, want 0755", mode)
	}
}

func TestExtractBinary_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/asset.tar.gz"
	writeTestTarGz(t, path, "README.md", []byte("docs"), 0o644)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	if _, _, err := ExtractBinary(f, "omac"); err == nil {
		t.Fatalf("expected error for missing binary entry")
	}
}
