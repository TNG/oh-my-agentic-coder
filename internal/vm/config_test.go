package vm

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadUserConfigMissingIsZero(t *testing.T) {
	cfg, err := LoadUserConfig(filepath.Join(t.TempDir(), "vm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, UserConfig{}) {
		t.Errorf("cfg = %+v, want zero", cfg)
	}
}

func TestLoadUserConfigParsesAllFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vm.yaml")
	os.WriteFile(p, []byte("cpus: 8\nmemory: 16GiB\ndisk: 80GiB\njdks: [temurin-25]\npackages: [jq]\n"), 0o600)
	cfg, err := LoadUserConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	want := UserConfig{CPUs: 8, Memory: "16GiB", Disk: "80GiB", JDKs: []string{"temurin-25"}, Packages: []string{"jq"}}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("cfg = %+v, want %+v", cfg, want)
	}
}

func TestLoadUserConfigRejectsUnknownField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vm.yaml")
	os.WriteFile(p, []byte("cpu: 8\n"), 0o600)
	if _, err := LoadUserConfig(p); err == nil {
		t.Error("unknown field accepted")
	}
}

func TestResolveResourcesDefaults(t *testing.T) {
	gib := uint64(1) << 30
	cases := []struct {
		name string
		host HostResources
		want Resources
	}{
		{"16 cores 64 GiB", HostResources{CPUs: 16, MemoryBytes: 64 * gib}, Resources{CPUs: 8, Memory: "16GiB", Disk: "60GiB"}},
		{"4 cores 8 GiB floors", HostResources{CPUs: 4, MemoryBytes: 8 * gib}, Resources{CPUs: 4, Memory: "8GiB", Disk: "60GiB"}},
		{"unknown host floors", HostResources{}, Resources{CPUs: 4, Memory: "8GiB", Disk: "60GiB"}},
		{"odd rounds down", HostResources{CPUs: 11, MemoryBytes: 37 * gib}, Resources{CPUs: 5, Memory: "9GiB", Disk: "60GiB"}},
	}
	for _, c := range cases {
		if got := ResolveResources(UserConfig{}, c.host); got != c.want {
			t.Errorf("%s: ResolveResources = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestResolveResourcesUserWins(t *testing.T) {
	got := ResolveResources(UserConfig{CPUs: 2, Memory: "3GiB", Disk: "10GiB"}, HostResources{CPUs: 16, MemoryBytes: 1 << 40})
	if want := (Resources{CPUs: 2, Memory: "3GiB", Disk: "10GiB"}); got != want {
		t.Errorf("ResolveResources = %+v, want %+v", got, want)
	}
}

func TestDetectHostReportsSomething(t *testing.T) {
	h := DetectHost()
	if h.CPUs < 1 {
		t.Errorf("CPUs = %d", h.CPUs)
	}
	// MemoryBytes may be 0 (unknown), but darwin and linux are expected to report a value.
}
