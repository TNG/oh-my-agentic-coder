package updater

import (
	"context"
	"io"
	"os"
	"os/exec"
)

// CommandRunner runs an external command. The real implementation wires
// stdin/stdout/stderr straight through to the terminal, so an interactive
// prompt (sudo's password prompt, brew's progress output) behaves exactly
// as if the user had typed the command themselves. Extra files are inherited
// by the child starting at fd 3, which lets an installer consume a verified
// artifact without reopening a path.
type CommandRunner interface {
	Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer, extraFiles ...*os.File) error
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer, extraFiles ...*os.File) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.ExtraFiles = extraFiles
	return cmd.Run()
}
