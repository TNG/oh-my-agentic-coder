//go:build linux

package sandboxrun

import "testing"

func TestGitMountAliasesCannotReopenProtectedTrees(t *testing.T) {
	base := "1 0 8:1 / / rw - ext4 /dev/root rw\n"
	for _, alias := range []string{
		"2 1 8:1 /repo /alias rw - ext4 /dev/root rw\n",
		"2 1 8:1 /repo/.git /repo/alias rw - ext4 /dev/root rw\n",
		"2 1 8:1 / /alias rw - ext4 /dev/root rw\n",
		"2 1 8:1 /usr /repo/tools rw - ext4 /dev/root rw\n",
		"2 1 8:1 /private /alias rw - ext4 /dev/root rw\n",
	} {
		g := &Grants{Workdir: "/repo", AllowPaths: []string{"/repo", "/alias"}, ProtectedPaths: []string{"/private/runtime"}}
		if err := checkGitMountAliases(g, base+alias); err == nil {
			t.Errorf("admitted writable alias: %s", alias)
		}
	}
	g := &Grants{Workdir: "/repo", AllowPaths: []string{"/repo", "/repo/.git/objects", "/cache"}}
	if err := checkGitMountAliases(g, base+"2 1 8:2 / /cache rw - ext4 /dev/other rw\n"); err != nil {
		t.Fatal(err)
	}
}
