package gitservice

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Process creation belongs exclusively to the platform worker, never handlers.
func TestGitProcessBoundary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		for _, violation := range gitProcessViolations(name, nil) {
			t.Errorf("%s: %s", name, violation)
		}
	}
}

func gitProcessViolations(name string, source any) []string {
	file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
	if err != nil {
		return []string{err.Error()}
	}
	var violations []string
	for _, imp := range file.Imports {
		if imp.Path.Value == `"os/exec"` && name != "worker_linux.go" {
			violations = append(violations, "only the confined worker may import os/exec")
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "StartProcess", "ForkExec", "Exec", "Command":
			violations = append(violations, "process creation must use the deadline-bound worker")
		case "CommandContext":
			if name != "worker_linux.go" || len(call.Args) < 2 {
				violations = append(violations, "process creation outside the worker")
			} else if exe, ok := call.Args[1].(*ast.BasicLit); !ok || exe.Value != `"/usr/bin/bwrap"` {
				violations = append(violations, "worker must enter bubblewrap, not execute host Git")
			}
		}
		return true
	})
	return violations
}

func TestGitProcessBoundaryRejectsUnsafeFixtures(t *testing.T) {
	for _, tc := range []struct {
		file, call string
		allowed    bool
	}{
		{"operations.go", `exec.Command("git", "add", ".")`, false},
		{"operations.go", `exec.CommandContext(ctx, "/usr/bin/bwrap")`, false},
		{"worker_linux.go", `exec.CommandContext(ctx, "/usr/bin/git")`, false},
		{"worker_linux.go", `exec.CommandContext(ctx, executable)`, false},
		{"worker_linux.go", `exec.CommandContext(ctx, "/usr/bin/bwrap")`, true},
	} {
		source := fmt.Sprintf("package gitservice; import exec \"os/exec\"; func run() { %s }", tc.call)
		if got := len(gitProcessViolations(tc.file, source)) == 0; got != tc.allowed {
			t.Errorf("%s %s: allowed=%t", tc.file, tc.call, got)
		}
	}
}
