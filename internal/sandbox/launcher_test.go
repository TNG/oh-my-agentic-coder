package sandbox

import (
	"os"
	"reflect"
	"testing"
)

// TestBuildBuiltinArgv checks the exact command produced for the builtin
// sandbox: the {{tmpdir}} grant pair is present when a temp dir is set and
// absent otherwise, and a missing inner command is an error.
func TestBuildBuiltinArgv(t *testing.T) {
	self, _ := os.Executable()

	// With a temp dir: the read+write grant pair is present.
	got, err := BuildBuiltinArgv(Inputs{
		Socket:   "/tmp/omac-abc/bridge.sock",
		TCPPort:  41017,
		InnerCmd: []string{"opencode", "--model", "opus"},
		TmpDir:   "/tmp/omac-sandbox-tmp-xyz",
	})
	if err != nil {
		t.Fatalf("BuildBuiltinArgv: %v", err)
	}
	want := []string{
		self, "sandbox", "run",
		"--profile", "default",
		"--allow-file", "/tmp/omac-abc/bridge.sock",
		"--read", "/tmp/omac-abc",
		"--read", "/tmp/omac-sandbox-tmp-xyz",
		"--write", "/tmp/omac-sandbox-tmp-xyz",
		"--open-port", "41017",
		"--",
		"opencode", "--model", "opus",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("with tmpdir mismatch\n got: %#v\nwant: %#v", got, want)
	}

	// Without a temp dir: the grant pair (read+write) is absent (no empty-path flags).
	got, err = BuildBuiltinArgv(Inputs{
		Socket:   "/tmp/omac-abc/bridge.sock",
		TCPPort:  41017,
		InnerCmd: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("BuildBuiltinArgv: %v", err)
	}
	want = []string{
		self, "sandbox", "run",
		"--profile", "default",
		"--allow-file", "/tmp/omac-abc/bridge.sock",
		"--read", "/tmp/omac-abc",
		"--open-port", "41017",
		"--",
		"claude",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("no-tmpdir mismatch\n got: %#v\nwant: %#v", got, want)
	}

	// No inner command is an error.
	if _, err := BuildBuiltinArgv(Inputs{Socket: "/s/bridge.sock"}); err == nil {
		t.Error("BuildBuiltinArgv with no inner_cmd should error")
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
