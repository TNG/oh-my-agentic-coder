package updater

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
)

// ExtractBinary reads a gzip-compressed tar stream and returns the content
// and mode of the first regular file entry named wantName.
func ExtractBinary(gz io.Reader, wantName string) ([]byte, fs.FileMode, error) {
	zr, err := gzip.NewReader(gz)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, 0, fmt.Errorf("binary %q not found in archive", wantName)
		}
		if err != nil {
			return nil, 0, err
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != wantName {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, 0, err
		}
		return data, fs.FileMode(hdr.Mode).Perm(), nil
	}
}
