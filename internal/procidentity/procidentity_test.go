package procidentity

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
)

// TestParseStartTime_ParenthesisedCommWithSpaces asserts the stat
// parser correctly handles a comm field that contains spaces (it is
// wrapped in parens in /proc/<pid>/stat), so the documented field-22
// index for `starttime` lines up after the closing paren.
func TestParseStartTime_ParenthesisedCommWithSpaces(t *testing.T) {
	// Real-ish /proc/<pid>/stat: comm "java (gradle)" has spaces.
	stat := "1234 (java (gradle)) S 1 1234 1234 0 -1 4194304 100 0 0 0 " +
		"1 2 0 0 20 0 1 0 123456 0 0 18446744073709551615 1 1 0 0 0 0"
	got, err := parseStartTime(stat)
	if err != nil {
		t.Fatalf("parseStartTime: %v", err)
	}
	if got != "123456" {
		t.Errorf("starttime = %q, want 123456", got)
	}
}

func TestParseStartTime_TooFewFields(t *testing.T) {
	stat := "1234 (java) S 1 1 1 0" // far fewer than 22 fields
	_, err := parseStartTime(stat)
	if err == nil {
		t.Fatal("expected error for too few fields, got nil")
	}
}

func TestParseStartTime_NonNumeric(t *testing.T) {
	// Build a stat with `notanumber` at field 22 (comm already
	// replaced by a placeholder when parsed, so count fields after
	// stripping the parens).
	// pid(1) comm(2) state(3) ... starttime(22)
	fields := []string{"1234", "(java)"}
	// fields 3..21 (19 fields) with valid ints, then field 22 =
	// "notanumber".
	for i := 0; i < 19; i++ {
		fields = append(fields, "1")
	}
	fields = append(fields, "notanumber")
	stat := strings.Join(fields, " ")
	_, err := parseStartTime(stat)
	if err == nil {
		t.Fatal("expected error for non-numeric starttime, got nil")
	}
}

// TestParseGradleMainClass_Found asserts the cmdline parser recognises
// the Gradle daemon bootstrap class whether it stands alone or appears
// after a jar path.
func TestParseGradleMainClass_Found(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{
			name: "bare class",
			line: "/usr/bin/java\x00-cp\x00/some/gradle.jar\x00" + GradleDaemonMainClass,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseGradleMainClass([]byte(c.line + "\x00"))
			if got != GradleDaemonMainClass {
				t.Errorf("got %q, want %q", got, GradleDaemonMainClass)
			}
		})
	}
}

func TestParseGradleMainClass_NotGradle(t *testing.T) {
	line := []byte("/usr/bin/java\x00-cp\x00foo.jar\x00org.something.Other\x00")
	if got := parseGradleMainClass(line); got != "" {
		t.Errorf("got %q, want empty for non-Gradle argv", got)
	}
}

func TestSplitNul(t *testing.T) {
	got := splitNul([]byte("a\x00bb\x00ccc\x00"))
	want := []string{"a", "bb", "ccc"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("tok[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestIdentify_SmokeSelf is a native smoke test: Identify on the test
// process's own pid must return a non-empty Executable and no error.
// Skipped on non-linux/darwin. Cannot assert main class (the test
// binary is not a Gradle daemon).
func TestIdentify_SmokeSelf(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("no native Identify on %s", runtime.GOOS)
	}
	id, err := Identify(os.Getpid())
	if err != nil {
		t.Fatalf("Identify(self): %v", err)
	}
	if id.Executable == "" {
		t.Error("Executable empty for self")
	}
	if id.StartIdentity == "" {
		t.Error("StartIdentity empty for self")
	}
}

// TestVerify_MatchRules uses a fake identify to assert the Verify match
// logic: executable mismatch, main-class missing, start-identity
// change (PID reuse), and the all-match happy path.
func TestVerify_MatchRules(t *testing.T) {
	saved := identify
	defer func() { identify = saved }()

	const wantExe = "/opt/jdk/bin/java"
	const wantStart = "99999"

	cases := []struct {
		name      string
		id        Identity
		idErr     error
		wantMatch bool
		wantErr   error
	}{
		{
			name: "all match",
			id: Identity{
				Executable:    wantExe,
				MainClass:     GradleDaemonMainClass,
				StartIdentity: wantStart,
			},
			wantMatch: true,
		},
		{
			name: "executable mismatch",
			id: Identity{
				Executable:    "/other/java",
				MainClass:     GradleDaemonMainClass,
				StartIdentity: wantStart,
			},
			wantMatch: false,
		},
		{
			name: "main class missing",
			id: Identity{
				Executable:    wantExe,
				MainClass:     "",
				StartIdentity: wantStart,
			},
			wantMatch: false,
		},
		{
			name: "start identity changed (PID reuse)",
			id: Identity{
				Executable:    wantExe,
				MainClass:     GradleDaemonMainClass,
				StartIdentity: "11111",
			},
			wantMatch: false,
		},
		{
			name: "no prior start identity (promote time)",
			id: Identity{
				Executable:    wantExe,
				MainClass:     GradleDaemonMainClass,
				StartIdentity: "anything",
			},
			wantMatch: true, // expectedStart empty -> don't check
		},
		{
			name:    "no such process",
			idErr:   ErrNoSuchProcess,
			wantErr: ErrNoSuchProcess,
		},
		{
			name:    "unverifiable propagates",
			idErr:   ErrUnverifiable,
			wantErr: ErrUnverifiable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			identify = func(int) (Identity, error) { return c.id, c.idErr }
			expectedStart := wantStart
			if c.name == "no prior start identity (promote time)" {
				expectedStart = ""
			}
			match, _, err := Verify(1, wantExe, expectedStart)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Errorf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if match != c.wantMatch {
				t.Errorf("match = %v, want %v", match, c.wantMatch)
			}
		})
	}
}
