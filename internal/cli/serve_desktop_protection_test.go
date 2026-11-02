package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/opencodestate"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestDesktopProtectionReportsInvalidSourcesUnderGrantedProject(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	source := filepath.Join(root, "drafts.sqlite")
	var out bytes.Buffer
	reportDesktopSkipped(&out, []opencodestate.SkippedWorktree{
		{Path: source, Reason: opencodestate.SkipInvalidSource},
		{Path: root, Reason: opencodestate.SkipUntrusted},
	}, []string{root})
	if !strings.Contains(out.String(), source) || !strings.Contains(out.String(), "invalid") {
		t.Fatalf("invalid source diagnostic was suppressed by a project grant: %q", out.String())
	}
	if strings.Contains(out.String(), string(opencodestate.SkipUntrusted)) {
		t.Fatalf("successfully granted worktree should still suppress ordinary rejection: %q", out.String())
	}
}

func TestDesktopProtectionRequiresEnforceableSandbox(t *testing.T) {
	native := sandboxPlan{Native: true, Known: true, Launcher: config.SandboxProfile{
		Command: []string{"{{self}}", "sandbox", "run", "--", "x"},
	}}
	for _, tc := range []struct {
		name                      string
		noSandbox, learn, noInner bool
		plan                      sandboxPlan
		wantErr                   bool
	}{
		{name: "native", plan: native},
		{name: "opaque", plan: sandboxPlan{}, wantErr: true},
		{name: "no sandbox", plan: native, noSandbox: true, wantErr: true},
		{name: "learn", plan: native, learn: true, wantErr: true},
		{name: "no inner launches no grants", noInner: true, noSandbox: true, learn: true},
		{name: "invalid policy", plan: sandboxPlan{Native: true, Known: true, PolicyErr: errors.New("bad policy")}, wantErr: true},
		{name: "template learn", plan: sandboxPlan{Native: true, Known: true, Launcher: config.SandboxProfile{
			Command: []string{"{{self}}", "sandbox", "run", "--learn", "--", "x"},
		}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDesktopSandbox(true, tc.noInner, tc.noSandbox, tc.learn, tc.plan)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "--for-opencode-desktop") {
				t.Fatal(err)
			}
			if err := validateDesktopSandbox(false, tc.noInner, tc.noSandbox, tc.learn, tc.plan); err != nil {
				t.Fatalf("non-Desktop launch changed: %v", err)
			}
		})
	}
}

func TestDesktopProtectionRunServeRefusesBeforeLaunching(t *testing.T) {
	for _, mode := range [][]string{{"--no-sandbox"}, {"--learn"}, {"--sandbox", "no-sandbox-debug"}} {
		t.Run(strings.Join(mode, " "), func(t *testing.T) {
			isolateHome(t)
			env, stdout, stderr, drain := newPipeEnv(t, "")
			env.Workdir = t.TempDir()
			args := append([]string{"--for-opencode-desktop", "--inner", "/bin/true"}, mode...)
			code := runServe(args, env)
			drain()
			if code != ExitConfigInvalid || !strings.Contains(stderr.String(), "--for-opencode-desktop requires the native sandbox") {
				t.Fatalf("unsafe Desktop launch not refused: exit=%d stderr=%s", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("control plane started before refusal: %q", stdout.String())
			}
		})
	}
}

func TestDesktopProtectionStartupLiteralDeniesForEveryStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config[1]$HOME"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data[1]$HOME"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state[1]$HOME"))
	for _, profile := range []string{"default", "existing-custom"} {
		t.Run(profile, func(t *testing.T) {
			// No stores exist, and an ancestor grant from an existing profile
			// must not prevent the startup denies from being installed.
			argv := []string{"omac", "sandbox", "run", "--profile", profile, "--allow", home, "--deny", "custom.secret", "--", "opencode", "serve"}
			projects := []string{filepath.Join(home, "project$HOME[1]"), filepath.Join(home, "other-project")}
			got, err := desktopProtectionArgv(argv, projects)
			if err != nil {
				t.Fatal(err)
			}
			flags, err := sandboxprofile.ParseFlags(got[3:])
			if err != nil {
				t.Fatal(err)
			}
			want := opencodestate.DesktopStorePaths()
			if len(want) == 0 {
				t.Fatal("store enumeration is empty")
			}
			if !reflect.DeepEqual(flags.DenyLiteral, want) {
				t.Fatalf("denies = %q, want %q", flags.DenyLiteral, want)
			}
			if !reflect.DeepEqual(flags.RequireProtectedPath, want) {
				t.Fatalf("required protection = %q, want %q", flags.RequireProtectedPath, want)
			}
			if !reflect.DeepEqual(flags.AllowLiteral, append(projects, filepath.Join(os.Getenv("XDG_STATE_HOME"), "opencode"))) {
				t.Fatalf("only literal project and opencode runtime grants wanted, got %q", flags.AllowLiteral)
			}
			if !reflect.DeepEqual(flags.Deny, []string{"custom.secret"}) {
				t.Fatalf("custom deny lost: %v", flags.Deny)
			}
			if !reflect.DeepEqual(flags.InnerArgv, []string{"opencode", "serve"}) {
				t.Fatal(flags.InnerArgv)
			}
		})
	}
}

func TestDesktopProtectionRejectsExpandedLearn(t *testing.T) {
	_, err := desktopProtectionArgv([]string{"omac", "sandbox", "run", "--learn", "--", "opencode"}, nil)
	if err == nil {
		t.Fatal("expanded launcher --learn must not erase startup denies")
	}
}
