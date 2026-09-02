package buildengine

import (
	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

// prepareSharedCacheScope mirrors cli.prepareBuildCache's default global
// scope path without importing internal/cli (which would be a cycle). It
// uses the same toolcache.PrepareShared path rooted at the isolated HOME
// so the engine's buildrun.GrantsFor resolves the same leaf layout the
// CLI does.
//
// Returns the resolved cache scope dir and a release func.
func prepareSharedCacheScope(workdir string) (string, func(), error) {
	scope, err := toolcache.PrepareShared()
	if err != nil {
		return "", nil, err
	}
	return scope.Dir, func() { _ = scope.Close() }, nil
}
