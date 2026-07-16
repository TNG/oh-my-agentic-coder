package updater

import (
	"context"
	"io"
	"os/exec"
)

// CommandRunner runs an external command. The real implementation wires
// stdin/stdout/stderr straight through to the terminal, so an interactive
// prompt (sudo's password prompt, brew's progress output) behaves exactly
// as if the user had typed the command themselves.
type CommandRunner interface {
	Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
