package sandboxrun

import (
	"os"
	"strings"
	"testing"
)

func TestTerminalScanAskAnswers(t *testing.T) {
	cases := []struct {
		input string
		want  ScanChoice
	}{
		{"c\n", ScanContinue},
		{"continue\n", ScanContinue},
		{"s\n", ScanStartIncomplete},
		{"start\n", ScanStartIncomplete},
		{"\n", ScanAbort},
		{"x\n", ScanAbort},
		{"a\n", ScanAbort},
		{"", ScanAbort},
	}
	for _, tc := range cases {
		var out strings.Builder
		ask := scanAskFrom(strings.NewReader(tc.input), &out, "/p/default.json")
		if got := ask([]string{"/home/x/monorepo"}, 1_000_000); got != tc.want {
			t.Errorf("input %q: choice = %v, want %v", tc.input, got, tc.want)
		}
		text := out.String()
		for _, want := range []string{
			"omac: the .env scan of /home/x/monorepo stopped after 1,000,000 entries.",
			"[c] continue scanning (next 1,000,000)",
			"[A] abort (default)",
			"/p/default.json",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("input %q: prompt %q missing %q", tc.input, text, want)
			}
		}
	}
}

func TestTerminalScanAskRepeatedQuestion(t *testing.T) {
	var out strings.Builder
	ask := scanAskFrom(strings.NewReader("c\ns\n"), &out, "")
	if got := ask([]string{"/a", "/b"}, 1000); got != ScanContinue {
		t.Fatalf("first answer = %v, want continue", got)
	}
	if got := ask([]string{"/a"}, 2000); got != ScanStartIncomplete {
		t.Fatalf("second answer = %v, want start", got)
	}
	text := out.String()
	for _, want := range []string{
		"the .env scan of /a, /b stopped after 1,000 entries.",
		"the .env scan of /a stopped after 2,000 entries.",
		"(next 1,000)",
		"your sandbox profile",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt %q missing %q", text, want)
		}
	}
	if strings.Contains(text, "(next 2,000)") {
		t.Errorf("prompt %q: continue must add the base limit, not the current one", text)
	}
}

func TestTerminalScanAskNoTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "in")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if TerminalScanAsk(f, f, "") != nil {
		t.Error("TerminalScanAsk on regular files must return nil")
	}
	if TerminalScanAsk(f, &strings.Builder{}, "") != nil {
		t.Error("TerminalScanAsk with a non-file writer must return nil")
	}
}
