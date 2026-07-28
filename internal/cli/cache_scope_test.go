package cli

import (
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

func TestResolveCacheScope(t *testing.T) {
	for _, c := range []struct {
		name     string
		cfg      config.CacheScope
		override string
		want     config.CacheScope
		wantErr  bool
	}{
		{name: "default global", want: config.CacheScopeGlobal},
		{name: "config value honored", cfg: config.CacheScopeWorkdir, want: config.CacheScopeWorkdir},
		{name: "flag overrides config", cfg: config.CacheScopeWorkdir, override: "global", want: config.CacheScopeGlobal},
		{name: "flag config scope", override: "config", want: config.CacheScopeConfig},
		{name: "invalid flag errors", override: "bogus", wantErr: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveCacheScope(config.CacheConfig{Scope: c.cfg}, c.override)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got scope %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveCacheScope: %v", err)
			}
			if got != c.want {
				t.Errorf("scope = %q, want %q", got, c.want)
			}
		})
	}
}
