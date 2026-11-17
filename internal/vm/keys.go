package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
)

// CommandRunner runs an external binary. Tests swap in a fake; production
// uses ExecRunner.
type CommandRunner func(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error

type commandEnvKey struct{}

// withCommandEnv adds NAME=VALUE entries to the environment of every
// command ExecRunner starts with the returned context.
func withCommandEnv(ctx context.Context, kv ...string) context.Context {
	return context.WithValue(ctx, commandEnvKey{}, kv)
}

func commandEnv(ctx context.Context) []string {
	kv, _ := ctx.Value(commandEnvKey{}).([]string)
	return kv
}

// ExecRunner runs name with args via os/exec, adding the environment set
// with withCommandEnv.
func ExecRunner(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	if kv := commandEnv(ctx); len(kv) > 0 {
		cmd.Env = append(os.Environ(), kv...)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// EnsureKeys creates the state directory with mode 0700 and generates the
// admin and agent ed25519 key pairs with ssh-keygen. A pair is left alone only
// when both its private key and its .pub file exist; a half-written pair is
// removed and generated again.
func EnsureKeys(ctx context.Context, l Layout, run CommandRunner) error {
	if err := os.MkdirAll(l.Root, 0o700); err != nil {
		return fmt.Errorf("omac vm: create %s: %w", l.Root, err)
	}
	// MkdirAll keeps the mode of an existing directory; tighten it.
	if err := os.Chmod(l.Root, 0o700); err != nil {
		return fmt.Errorf("omac vm: chmod %s: %w", l.Root, err)
	}
	keys := []struct{ path, comment string }{
		{l.AdminKey, "omac-vm-admin"},
		{l.AgentKey, "omac-vm-agent"},
	}
	for _, k := range keys {
		complete, err := keyPairExists(k.path)
		if err != nil {
			return err
		}
		if complete {
			continue
		}
		// ssh-keygen writes the private key first and the .pub second, so an
		// interrupted run leaves half a pair. Remove it: ssh-keygen would
		// otherwise ask whether to overwrite an existing private key.
		if err := removeKeyPair(k.path); err != nil {
			return err
		}
		args := []string{"-q", "-t", "ed25519", "-N", "", "-C", k.comment, "-f", k.path}
		if err := run(ctx, "ssh-keygen", args, nil, nil, os.Stderr); err != nil {
			_ = removeKeyPair(k.path)
			return fmt.Errorf("omac vm: ssh-keygen %s: %w", k.path, err)
		}
	}
	return nil
}

// keyPairExists reports whether both the private key and its .pub file exist.
func keyPairExists(path string) (bool, error) {
	for _, p := range []string{path, path + ".pub"} {
		if _, err := os.Stat(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return false, nil
			}
			return false, fmt.Errorf("omac vm: stat %s: %w", p, err)
		}
	}
	return true, nil
}

func removeKeyPair(path string) error {
	for _, p := range []string{path, path + ".pub"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("omac vm: remove key file %s: %w", p, err)
		}
	}
	return nil
}

// ReadPubKey returns the single-line public key stored at path+".pub",
// without surrounding whitespace.
func ReadPubKey(path string) (string, error) {
	raw, err := os.ReadFile(path + ".pub")
	if err != nil {
		return "", fmt.Errorf("omac vm: read public key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return "", fmt.Errorf("omac vm: %s.pub is not a single-line public key", path)
	}
	return key, nil
}
