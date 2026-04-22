// Package secrets provides a Secret type with zeroize-on-free semantics
// and helpers for safely collecting secret input from users.
package secrets

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// Secret is a write-mostly credential. Its Debug/Format output is redacted
// and its underlying bytes can be wiped with Zero.
//
// Go has no secure memory API; Zero is best-effort (the runtime may have
// already copied the bytes elsewhere). The goal is to close the common
// leakage paths (%v/%s formatting, logs, accidental JSON marshaling).
type Secret struct {
	b []byte
}

// NewSecret copies the given bytes into a Secret. The caller may zero its
// own buffer afterwards.
func NewSecret(b []byte) Secret {
	cp := make([]byte, len(b))
	copy(cp, b)
	return Secret{b: cp}
}

// NewSecretString copies s into a Secret.
func NewSecretString(s string) Secret {
	return Secret{b: []byte(s)}
}

// Expose returns the underlying bytes. Do not store the returned slice.
func (s Secret) Expose() []byte { return s.b }

// ExposeString returns the secret as a string. Prefer Expose for byte use.
func (s Secret) ExposeString() string { return string(s.b) }

// IsEmpty reports whether the secret has no content.
func (s Secret) IsEmpty() bool { return len(s.b) == 0 }

// Zero wipes the secret's buffer.
func (s *Secret) Zero() {
	for i := range s.b {
		s.b[i] = 0
	}
	s.b = nil
}

// String implements fmt.Stringer; always redacts.
func (s Secret) String() string { return "<redacted>" }

// GoString implements fmt.GoStringer; always redacts.
func (s Secret) GoString() string { return "<redacted>" }

// MarshalJSON prevents accidental JSON serialization.
func (s Secret) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("refusing to marshal secret")
}

// ReadPassword reads a line from stdin with no echo when possible.
// If stdin is not a terminal it falls back to echoed input after warning on stderr.
func ReadPassword(prompt string) (Secret, error) {
	fmt.Fprint(os.Stderr, prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		raw, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return Secret{}, fmt.Errorf("read password: %w", err)
		}
		return Secret{b: raw}, nil
	}
	fmt.Fprintln(os.Stderr, "\n[warning] stdin is not a terminal; input will be echoed")
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			line = append(line, buf[0])
		}
		if err != nil {
			break
		}
	}
	return Secret{b: line}, nil
}
