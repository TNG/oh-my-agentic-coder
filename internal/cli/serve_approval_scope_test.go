package cli

import (
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/facade"
)

func TestServeApprovalScope(t *testing.T) {
	a := &dirState{Dir: "/p/a", Token: "tok-a"}
	b := &dirState{Dir: "/p/b", Token: "tok-b"}
	two := &serveServer{dirs: map[string]*dirState{a.Dir: a, b.Dir: b}, byToken: map[string]*dirState{a.Token: a, b.Token: b}}
	one := &serveServer{dirs: map[string]*dirState{a.Dir: a}, byToken: map[string]*dirState{a.Token: a}}
	for name, c := range map[string]struct {
		srv       *serveServer
		namespace string
		want      string
	}{
		"global skill is shared":           {two, facade.GlobalNamespace, "global"},
		"project namespace":                {two, "tok-b", workdirScope("/p/b")},
		"flat alias with one directory":    {one, "", workdirScope("/p/a")},
		"flat alias with two directories":  {two, "", ""},
		"unknown namespace stores nothing": {two, "tok-gone", ""},
	} {
		if got := c.srv.approvalScope(c.namespace); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}
