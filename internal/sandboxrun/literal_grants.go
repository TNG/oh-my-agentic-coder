package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
)

func existingLiteralGrants(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("literal grant %q must be absolute", p)
		}
		p = filepath.Clean(p)
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("literal grant %q: %w", p, err)
		}
		if resolved := canonicalPath(p); resolved == "/" {
			return nil, fmt.Errorf("literal grant %q resolves to the filesystem root; grant only the required subtree", p)
		}
		out = append(out, p)
	}
	return out, nil
}
