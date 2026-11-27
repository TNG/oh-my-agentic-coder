package gitservice

import "testing"

func TestSupportedConfigRejectsIncompatibleSemantics(t *testing.T) {
	for _, setting := range []string{
		"extensions.objectformat\nsha256", "core.symlinks\nfalse", "core.eol\ncrlf",
		"core.attributesfile\noutside", "include.path\noutside", "includeif.onbranch:main.path\noutside",
		"core.autocrlf\ntrue", "core.ignorecase\ntrue", "core.worktree\n../outside",
		"core.sparsecheckout\ntrue", "remote.origin.promisor\ntrue",
	} {
		t.Run(setting, func(t *testing.T) {
			if _, err := supportedConfig([]byte("core.repositoryformatversion\n0\x00" + setting + "\x00")); err == nil {
				t.Fatal("unsupported semantics silently ignored")
			}
		})
	}
	if filemode, err := supportedConfig([]byte("core.repositoryformatversion\n0\x00core.filemode\nfalse\x00core.symlinks\ntrue\x00core.eol\nlf\x00")); err != nil || filemode {
		t.Fatalf("supported settings: %v %v", filemode, err)
	}
}
