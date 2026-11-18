package vm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestGitRunsOnlyThroughRunGit fails when a non-test Go file of the module
// holds the string "git", or a path ending in "/git", outside the function
// runGit in internal/vm/exec.go. Such a string would start git without
// runGit's hardening against git config the sandbox planted. The test
// cannot see git started through a shell string or a name built at
// runtime.
func TestGitRunsOnlyThroughRunGit(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	sawRunGit := false
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "runGit" && filepath.ToSlash(rel) == "internal/vm/exec.go" {
				sawRunGit = true
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if v, err := strconv.Unquote(lit.Value); err == nil && (v == "git" || strings.HasSuffix(v, "/git")) {
					t.Errorf("%s: %s outside vm.runGit", fset.Position(lit.Pos()), lit.Value)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawRunGit {
		t.Fatal("found no func runGit in internal/vm/exec.go")
	}
}

// moduleRoot is the directory of the go.mod above the test's directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("found no go.mod above the test directory")
		}
		dir = parent
	}
}
