package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

func TestRouteHealthPath(t *testing.T) {
	cases := map[string]struct {
		meta *config.Meta
		want string
	}{
		"no manifest":       {nil, "/status"},
		"no sidecar":        {&config.Meta{}, "/status"},
		"no health section": {&config.Meta{Sidecar: &config.SidecarMeta{}}, "/status"},
		"custom path":       {&config.Meta{Sidecar: &config.SidecarMeta{Health: &config.HealthSpec{Path: "/healthz"}}}, "/healthz"},
	}
	for name, c := range cases {
		if got := routeHealthPath(c.meta); got != c.want {
			t.Errorf("%s: routeHealthPath = %q, want %q", name, got, c.want)
		}
	}
}

// Every route this package builds for a sidecar must name the sidecar's
// health path, or its health check queues behind the request cap.
func TestSidecarRoutesSetHealthPath(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	routes := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isFacadeRoute(lit.Type) {
				return true
			}
			keys := map[string]bool{}
			for _, elt := range lit.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok {
						keys[id.Name] = true
					}
				}
			}
			if keys["UpstreamPort"] {
				routes++
				if !keys["HealthPath"] {
					t.Errorf("%s: facade.Route with UpstreamPort but no HealthPath", fset.Position(lit.Pos()))
				}
			}
			return true
		})
	}
	if routes == 0 {
		t.Fatal("found no facade.Route literal with UpstreamPort; the check no longer sees the routes")
	}
}

func isFacadeRoute(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Route" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "facade"
}
