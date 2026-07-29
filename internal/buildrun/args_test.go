package buildrun

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	for _, c := range []struct {
		name     string
		args     []string
		wantRoot string
		wantArgs []string
		wantErr  string // substring; "" means no error
	}{
		{
			name:     "root flag with space",
			args:     []string{"--root", "backend", "--", "gradle", ":help"},
			wantRoot: "backend",
			wantArgs: []string{":help"},
		},
		{
			name:     "root flag with equals",
			args:     []string{"--root=backend", "--", "gradle", "test"},
			wantRoot: "backend",
			wantArgs: []string{"test"},
		},
		{
			name:     "no root defaults to dot",
			args:     []string{"--", "gradle", ":help"},
			wantRoot: ".",
			wantArgs: []string{":help"},
		},
		{
			name:     "pass-through args keep flags and values",
			args:     []string{"--root", "backend", "--", "gradle", "test", "--tests", "com.example.Foo", "--scan"},
			wantRoot: "backend",
			wantArgs: []string{"test", "--tests", "com.example.Foo", "--scan"},
		},
		{
			name:     "gradle with no task args",
			args:     []string{"--root", "backend", "--", "gradle"},
			wantRoot: "backend",
			wantArgs: nil,
		},
		{
			name:    "missing separator",
			args:    []string{"--root", "backend", "gradle", ":help"},
			wantErr: "separator",
		},
		{
			name:    "missing adapter token",
			args:    []string{"--root", "backend", "--"},
			wantErr: "adapter token",
		},
		{
			name:    "maven adapter rejected with seam error",
			args:    []string{"--root", "backend", "--", "maven", "verify"},
			wantErr: `unsupported adapter "maven"`,
		},
		{
			name:    "root requires a value",
			args:    []string{"--root", "--", "gradle"},
			wantErr: "--root requires a value", // "--" separates first; --root has no flag-side value
		},
		{
			name:    "unknown flag before separator",
			args:    []string{"--verbose", "--", "gradle"},
			wantErr: `unknown flag "--verbose"`,
		},
		{
			name:    "empty root value rejected",
			args:    []string{"--root=", "--", "gradle"},
			wantErr: "must not be empty",
		},
		{
			name:     "root-looking arg after separator belongs to gradle",
			args:     []string{"--", "gradle", "--root"},
			wantRoot: ".",
			wantArgs: []string{"--root"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := ParseArgs(c.args)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got none", c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), c.wantErr)
				}
				var reqErr *RequestError
				if !errors.As(err, &reqErr) {
					t.Errorf("error type = %T, want *RequestError", err)
				}
				if !errors.Is(err, errRequest) {
					t.Errorf("errors.Is(errRequest) = false, want true")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseArgs: %v", err)
			}
			if r.Root != c.wantRoot {
				t.Errorf("Root = %q, want %q", r.Root, c.wantRoot)
			}
			if len(r.Args) != 0 || len(c.wantArgs) != 0 {
				if !reflect.DeepEqual(r.Args, c.wantArgs) {
					t.Errorf("Args = %v, want %v", r.Args, c.wantArgs)
				}
			}
		})
	}
}

func TestParseArgsDanglingRootFlagValue(t *testing.T) {
	// "--root" as the final flag before missing separator context.
	_, err := ParseArgs([]string{"--root"})
	if err == nil || !strings.Contains(err.Error(), "separator") {
		t.Errorf("err = %v, want missing-separator error", err)
	}
}

func TestExitCode(t *testing.T) {
	execErr := errors.New("exec sandbox-exec: no such file")
	for _, c := range []struct {
		name      string
		buildExit int
		cancelled bool
		err       error
		want      int
	}{
		{"build success", 0, false, nil, 0},
		{"build failure passes through", 1, false, nil, 1},
		{"build failure 42 passes through", 42, false, nil, 42},
		{"build signal kill 130 distinct when not cancelled", 130, false, nil, 130},
		{"cancellation maps signal kill to 4", 130, true, nil, 4},
		{"cancellation maps 0 to 4", 0, true, nil, 4},
		{"service failure beats everything", 1, false, execErr, ExitServiceFailure},
		{"service failure beats cancelled", 130, true, execErr, ExitServiceFailure},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ExitCode(c.buildExit, c.cancelled, c.err); got != c.want {
				t.Errorf("ExitCode(%d, %v, %v) = %d, want %d", c.buildExit, c.cancelled, c.err, got, c.want)
			}
		})
	}
}
