package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/toolcache"
)

func TestInjectedToolCacheEnv(t *testing.T) {
	cacheDir := t.TempDir()
	grants := &Grants{AllowPaths: []string{cacheDir}}

	t.Run("empty cache directory injects nothing", func(t *testing.T) {
		got, err := injectedToolCacheEnv(grants, func(key string) string {
			if key == "OMAC_CACHE_MODE" {
				return "invalid"
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("injected = %v, want empty", got)
		}
	})

	t.Run("rejects invalid mode", func(t *testing.T) {
		_, err := injectedToolCacheEnv(grants, func(key string) string {
			switch key {
			case "OMAC_CACHE_DIR":
				return cacheDir
			case "OMAC_CACHE_MODE":
				return "shared"
			default:
				return ""
			}
		})
		if err == nil || !strings.Contains(err.Error(), "OMAC_CACHE_MODE") {
			t.Fatalf("error = %v, want invalid OMAC_CACHE_MODE error", err)
		}
	})

	t.Run("requires an exact writable grant", func(t *testing.T) {
		child := filepath.Join(cacheDir, "child")
		if err := os.Mkdir(child, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := injectedToolCacheEnv(&Grants{AllowPaths: []string{filepath.Dir(cacheDir)}}, func(key string) string {
			switch key {
			case "OMAC_CACHE_DIR":
				return child
			case "OMAC_CACHE_MODE":
				return string(toolcache.ModePersistent)
			default:
				return ""
			}
		})
		if err == nil || !strings.Contains(err.Error(), "writable grant") {
			t.Fatalf("error = %v, want missing writable grant error", err)
		}
	})

	t.Run("rejects an allowed regular file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "cache-file")
		if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := injectedToolCacheEnv(&Grants{AllowPaths: []string{file}}, func(key string) string {
			switch key {
			case "OMAC_CACHE_DIR":
				return file
			case "OMAC_CACHE_MODE":
				return string(toolcache.ModePersistent)
			default:
				return ""
			}
		})
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("error = %v, want non-directory cache path error", err)
		}
	})

	t.Run("canonicalizes matching directory and regenerates tool paths", func(t *testing.T) {
		linkedParent := t.TempDir()
		linked := filepath.Join(linkedParent, "cache")
		if err := os.Symlink(cacheDir, linked); err != nil {
			t.Fatal(err)
		}
		cachePath := cacheDir + string(filepath.Separator) + "."
		got, err := injectedToolCacheEnv(&Grants{AllowPaths: []string{linked}}, func(key string) string {
			switch key {
			case "OMAC_CACHE_DIR":
				return cachePath
			case "OMAC_CACHE_MODE":
				return string(toolcache.ModeEphemeral)
			case "GOCACHE":
				return "/hostile/go-build"
			case "CARGO_HOME":
				return "/hostile/cargo"
			default:
				return ""
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		canonicalCacheDir, err := filepath.EvalSymlinks(cacheDir)
		if err != nil {
			t.Fatal(err)
		}
		want := toolcache.Environment(canonicalCacheDir, toolcache.ModeEphemeral)
		if len(got) != len(want) {
			t.Fatalf("injected = %v, want %v", got, want)
		}
		for key, value := range want {
			if got[key] != value {
				t.Errorf("%s = %q, want %q", key, got[key], value)
			}
		}
	})
}

func TestCacheEnvSurvivesAllowVars(t *testing.T) {
	cacheDir := t.TempDir()
	injected, err := injectedToolCacheEnv(&Grants{AllowPaths: []string{cacheDir}}, func(key string) string {
		switch key {
		case "OMAC_CACHE_DIR":
			return cacheDir
		case "OMAC_CACHE_MODE":
			return string(toolcache.ModePersistent)
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	got := sandboxprofile.FilterEnv([]string{
		"HOME=/home/test",
		"GOCACHE=/hostile/go-build",
		"CARGO_HOME=/hostile/cargo",
	}, []string{"HOME"}, nil, injected)
	gotMap := envMap(got)
	if len(gotMap) != len(injected)+1 {
		t.Fatalf("environment = %v, want HOME plus all injected cache values", gotMap)
	}
	for key, value := range injected {
		if gotMap[key] != value {
			t.Errorf("%s = %q, want %q", key, gotMap[key], value)
		}
	}
}

func envMap(env []string) map[string]string {
	result := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

// TestWarnEmptyAllowVars covers the warning on the `omac sandbox run` path.
// The empty-allow_vars fail-closed behavior lands only on this entry point
// (omac start/serve already seed the defaults in their own layer), and it
// previously had no warning surface here.
func TestWarnEmptyAllowVars(t *testing.T) {
	t.Run("empty list warns", func(t *testing.T) {
		var buf strings.Builder
		warnEmptyAllowVars(&buf, nil)
		got := buf.String()
		if got == "" {
			t.Fatal("empty allow_vars produced no warning")
		}
		for _, want := range []string{"empty environment.allow_vars", "will not", "authenticate", "--allow-env"} {
			if !strings.Contains(got, want) {
				t.Errorf("warning lacks %q:\n%s", want, got)
			}
		}
	})

	t.Run("non-empty list is quiet", func(t *testing.T) {
		var buf strings.Builder
		warnEmptyAllowVars(&buf, []string{"HOME"})
		if buf.String() != "" {
			t.Errorf("non-empty allow_vars warned: %s", buf.String())
		}
	})

	// A start/serve-seeded launch reaches Run as --allow-env flags over an
	// empty profile. Merge folds those into allow_vars, so the inner run
	// must stay quiet rather than repeat the warning the launcher already
	// printed. Asserted through Merge (not a literal list) so the two
	// layers cannot drift apart.
	t.Run("seeded via --allow-env is quiet", func(t *testing.T) {
		empty := &sandboxprofile.Profile{Meta: sandboxprofile.Meta{Name: "empty"}}
		flags := &sandboxprofile.Flags{AllowEnv: sandboxprofile.DefaultAllowVars()}
		merged, _ := sandboxprofile.Merge(empty, flags)
		if len(merged.Environment.AllowVars) == 0 {
			t.Fatal("Merge dropped --allow-env entries; a seeded launch would warn twice")
		}
		var buf strings.Builder
		warnEmptyAllowVars(&buf, merged.Environment.AllowVars)
		if buf.String() != "" {
			t.Errorf("seeded launch warned: %s", buf.String())
		}
	})
}
