package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/approver"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
)

func TestServeApprovalScope(t *testing.T) {
	a := &dirState{Dir: "/p/a", Token: "tok-a", scope: projectScope("/p/a")}
	b := &dirState{Dir: "/p/b", Token: "tok-b", scope: projectScope("/p/b")}
	two := &serveServer{dirs: map[string]*dirState{a.Dir: a, b.Dir: b}, byToken: map[string]*dirState{a.Token: a, b.Token: b}}
	one := &serveServer{dirs: map[string]*dirState{a.Dir: a}, byToken: map[string]*dirState{a.Token: a}}
	for name, c := range map[string]struct {
		srv       *serveServer
		namespace string
		want      approver.Scope
	}{
		"global skill is shared":           {two, facade.GlobalNamespace, globalScope},
		"project namespace":                {two, "tok-b", projectScope("/p/b")},
		"flat alias with one directory":    {one, "", projectScope("/p/a")},
		"flat alias with two directories":  {two, "", approver.Scope{}},
		"unknown namespace stores nothing": {two, "tok-gone", approver.Scope{}},
	} {
		if got := c.srv.approvalScope(c.namespace); got != c.want {
			t.Errorf("%s: got %+v, want %+v", name, got, c.want)
		}
	}
}

// omac start resolves symlinks in the workdir and omac serve takes the
// directory as given, so the scope resolves them itself: both see one
// project.
func TestProjectScopeResolvesSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	resolved, _ := filepath.EvalSymlinks(real)
	got, want := projectScope(link), projectScope(real)
	if got != want || got.Dir != resolved || len(got.Key) != len("wd-")+24 {
		t.Errorf("via link %+v, direct %+v", got, want)
	}
}
