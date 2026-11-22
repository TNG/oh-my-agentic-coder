package mcpreg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/sidecarauth"
)

var envNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ParseDefinition decodes and validates the content of an mcp.yaml file.
// Unknown keys are an error. A command that is a bare program name is looked
// up on PATH when the server starts; a command with a directory part must be
// absolute.
func ParseDefinition(raw []byte) (*Definition, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var def Definition
	if err := dec.Decode(&def); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("definition is empty")
		}
		return nil, fmt.Errorf("parse definition: %w", err)
	}
	if err := def.validate(); err != nil {
		return nil, err
	}
	return &def, nil
}

func (d *Definition) validate() error {
	if err := config.ValidSkillName(d.Name); err != nil {
		return fmt.Errorf("name: %w", err)
	}
	if len(d.Server.Command) == 0 || d.Server.Command[0] == "" {
		return errors.New("server.command must not be empty")
	}
	if prog := d.Server.Command[0]; strings.ContainsRune(prog, filepath.Separator) && !filepath.IsAbs(prog) {
		return fmt.Errorf("server.command %q is a relative path; use an absolute path or a program name on PATH", prog)
	}
	seen := map[string]bool{}
	for _, name := range d.Secrets {
		if err := checkEnvName("secret", name); err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("secret %q is declared twice", name)
		}
		seen[name] = true
	}
	for name := range d.Server.Env {
		if err := checkEnvName("env", name); err != nil {
			return err
		}
	}
	for _, name := range d.Server.EnvPassthrough {
		if err := checkEnvName("env_passthrough", name); err != nil {
			return err
		}
	}
	if d.Mode != nil {
		if err := checkEnvName("mode.name", d.Mode.Name); err != nil {
			return err
		}
	}
	return d.validatePolicy()
}

func (d *Definition) validatePolicy() error {
	if d.Access != "" && !d.Access.Valid() {
		return fmt.Errorf("access %q must be one of %s, %s, %s", d.Access,
			mcppolicy.ReadOnly, mcppolicy.ReadWrite, mcppolicy.ReadWriteDelete)
	}
	tools := map[string]bool{}
	for i, t := range d.Tools {
		if t.Name == "" {
			return fmt.Errorf("tools[%d]: name must not be empty", i)
		}
		if tools[t.Name] {
			return fmt.Errorf("tool %q is listed twice", t.Name)
		}
		tools[t.Name] = true
		if !t.Class.Valid() {
			return fmt.Errorf("tool %q: class %q must be one of read, write, delete, off", t.Name, t.Class)
		}
	}
	if a := d.Approvals; a != nil {
		for _, m := range []struct {
			class string
			mode  approval.Mode
		}{{"read", a.Read}, {"write", a.Write}, {"delete", a.Delete}} {
			if m.mode != "" && !m.mode.Valid() {
				return fmt.Errorf("approvals.%s %q must be %s or %s", m.class, m.mode, approval.Required, approval.None)
			}
		}
	}
	if d.Review != nil && d.Review.ToolsHash != mcppolicy.ListHash(d.Review.Tools) {
		return errors.New("reviewed.tools_hash does not match reviewed.tools")
	}
	return nil
}

func checkEnvName(kind, name string) error {
	if !envNameRE.MatchString(name) {
		return fmt.Errorf("%s %q must match %s", kind, name, envNameRE)
	}
	if sidecarauth.Reserved(name) {
		return fmt.Errorf("%s %q is reserved", kind, name)
	}
	return nil
}

// CheckCommandOutsideWorkdir refuses a server whose absolute command lies
// inside workdir, because the agent can write there and would control what
// runs outside the sandbox.
func CheckCommandOutsideWorkdir(def *Definition, workdir string) error {
	prog := def.Server.Command[0]
	if !filepath.IsAbs(prog) || workdir == "" {
		return nil
	}
	rel, err := filepath.Rel(resolveExisting(workdir), resolveExisting(prog))
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("server.command %q is inside the workdir %q, which the agent can modify", prog, workdir)
	}
	return nil
}

// resolveExisting resolves symlinks in the longest existing prefix of path,
// so a command that does not exist yet is still compared by its real location.
func resolveExisting(path string) string {
	path = filepath.Clean(path)
	var tail []string
	for p := path; ; p = filepath.Dir(p) {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{r}, tail...)...)
		}
		if filepath.Dir(p) == p {
			return path
		}
		tail = append([]string{filepath.Base(p)}, tail...)
	}
}
