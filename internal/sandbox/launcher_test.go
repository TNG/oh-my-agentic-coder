package sandbox

import (
	"reflect"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

// externalProfile is a synthetic non-native launcher template used to
// exercise the profile-agnostic parts of Expand (the {{tmpdir_flags}} and
// {{per_skill_env_flags}} splats in particular) without depending on any
// shipped profile.
func externalProfile() config.SandboxProfile {
	return config.SandboxProfile{
		Command: []string{
			"external-sbx", "run",
			"--allow-file", "{{socket}}",
			"--read", "{{socket_dir}}",
			"{{per_skill_env_flags}}",
			"{{tmpdir_flags}}",
			"--open-port", "{{tcp_port}}",
			"--",
			"{{inner_cmd}}", "{{inner_args}}",
		},
	}
}

// TestExpand_NoTmpDir asserts that when no TmpDir is configured, the
// {{tmpdir_flags}} splat vanishes entirely (no `--read ""`/`--write ""`
// with empty paths, which would hand the launcher unusable arguments).
func TestExpand_NoTmpDir(t *testing.T) {
	got, err := Expand(externalProfile(), Inputs{
		Workdir:  "/work",
		Socket:   "/tmp/omac-abc/bridge.sock",
		TCPPort:  41017,
		InnerCmd: []string{"opencode"},
		// TmpDir intentionally empty.
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	for i, a := range got {
		if a == "" {
			t.Fatalf("argv[%d] is an empty string; tmpdir flags leaked: %#v", i, got)
		}
	}
	want := []string{
		"external-sbx", "run",
		"--allow-file", "/tmp/omac-abc/bridge.sock",
		"--read", "/tmp/omac-abc",
		"--open-port", "41017",
		"--",
		"opencode",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expand mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestExpand_TmpDir asserts that a configured TmpDir expands to the
// read+write grant pair via the {{tmpdir_flags}} splat.
func TestExpand_TmpDir(t *testing.T) {
	got, err := Expand(externalProfile(), Inputs{
		Workdir:  "/work",
		Socket:   "/tmp/omac-abc/bridge.sock",
		TCPPort:  41017,
		InnerCmd: []string{"opencode", "--model", "opus"},
		TmpDir:   "/tmp/omac-sandbox-tmp-xyz",
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := []string{
		"external-sbx", "run",
		"--allow-file", "/tmp/omac-abc/bridge.sock",
		"--read", "/tmp/omac-abc",
		"--read", "/tmp/omac-sandbox-tmp-xyz",
		"--write", "/tmp/omac-sandbox-tmp-xyz",
		"--open-port", "41017",
		"--",
		"opencode", "--model", "opus",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expand mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestExpand_NoMounts asserts that the launcher template substitution
// produces a valid argv when no skills are registered (Mounts is empty).
// This is the common case immediately after install: `omac start` should
// still bring up a sandbox so the user can iterate on inner commands
// before they decide which skills to register.
//
// Specifically, the {{per_skill_env_flags}} splat must expand to nothing
// (rather than e.g. erroring or leaving a literal token in the argv).
func TestExpand_NoMounts(t *testing.T) {
	got, err := Expand(externalProfile(), Inputs{
		Workdir:  "/work",
		Socket:   "/tmp/omac-abc/bridge.sock",
		TCPPort:  41017,
		Mounts:   nil,
		InnerCmd: []string{"opencode"},
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	for i, a := range got {
		if a == "" {
			t.Fatalf("argv[%d] is an empty string; per-skill flags leaked: %#v", i, got)
		}
	}
	want := []string{
		"external-sbx", "run",
		"--allow-file", "/tmp/omac-abc/bridge.sock",
		"--read", "/tmp/omac-abc",
		"--open-port", "41017",
		"--",
		"opencode",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expand mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestOmacEnvName(t *testing.T) {
	cases := map[string]string{
		"slack":          "OMAC_SLACK_BASE",
		"himalaya-email": "OMAC_HIMALAYA_EMAIL_BASE",
		"mail2":          "OMAC_MAIL2_BASE",
		"a-b_c":          "OMAC_A_B_C_BASE",
	}
	for in, want := range cases {
		if got := OmacEnvName(in); got != want {
			t.Errorf("OmacEnvName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOmacEnvValuesHaveNoTrailingSlash(t *testing.T) {
	if got, want := OmacTCPEnvValue("tng-slack", 41017), "http://127.0.0.1:41017/tng-slack"; got != want {
		t.Errorf("OmacTCPEnvValue() = %q, want %q", got, want)
	}
	if got, want := OmacEnvValue("tng-slack", "/tmp/omac/bridge.sock"), "http+unix://%2Ftmp%2Fomac%2Fbridge.sock/tng-slack"; got != want {
		t.Errorf("OmacEnvValue() = %q, want %q", got, want)
	}
}

func TestOmacEnvNameNamespaced(t *testing.T) {
	if got, want := OmacDirEnvName("a17f3c", "slack"), "OMAC_D_A17F3C_SLACK_BASE"; got != want {
		t.Errorf("OmacDirEnvName() = %q, want %q", got, want)
	}
	if got, want := OmacGlobalEnvName("weather"), "OMAC_G_WEATHER_BASE"; got != want {
		t.Errorf("OmacGlobalEnvName() = %q, want %q", got, want)
	}
}

func TestOmacTCPEnvValueNS(t *testing.T) {
	if got, want := OmacTCPEnvValueNS("a17f3c", "slack", 41017), "http://127.0.0.1:41017/a17f3c/slack"; got != want {
		t.Errorf("OmacTCPEnvValueNS(dir) = %q, want %q", got, want)
	}
	if got, want := OmacTCPEnvValueNS("__global__", "weather", 41017), "http://127.0.0.1:41017/__global__/weather"; got != want {
		t.Errorf("OmacTCPEnvValueNS(global) = %q, want %q", got, want)
	}
	if got, want := OmacTCPEnvValueNS("", "flat", 41017), "http://127.0.0.1:41017/flat"; got != want {
		t.Errorf("OmacTCPEnvValueNS(flat) = %q, want %q", got, want)
	}
}
