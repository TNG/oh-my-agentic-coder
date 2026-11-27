package mcpreg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcphttp"
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
	if d.Server.Remote() {
		if err := d.validateRemote(); err != nil {
			return err
		}
	} else {
		if len(d.Server.Command) == 0 || d.Server.Command[0] == "" {
			return errors.New("server.command must not be empty (or set server.url)")
		}
		if prog := d.Server.Command[0]; strings.ContainsRune(prog, filepath.Separator) && !filepath.IsAbs(prog) {
			return fmt.Errorf("server.command %q is a relative path; use an absolute path or a program name on PATH", prog)
		}
		if len(d.Server.Headers) > 0 || d.Server.BearerSecret != "" || d.Server.AllowPrivateNetwork {
			return errors.New("server.headers, server.bearer_secret and server.allow_private_network apply to server.url only")
		}
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
	if err := d.checkNoOverlap(); err != nil {
		return err
	}
	return d.validatePolicy()
}

var (
	headerNameRE = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	// credentialHeaderHints are fragments of header names that carry credentials.
	// Such a header cannot be written into the definition.
	credentialHeaderHints = []string{"auth", "token", "secret", "key", "cookie", "password", "credential"}
	reservedHeaders       = map[string]bool{"host": true, "content-length": true, "content-type": true, "accept": true,
		"transfer-encoding": true, "connection": true, "upgrade": true, "mcp-session-id": true, "mcp-protocol-version": true}
)

// validateRemote checks a definition whose server is reached by URL.
func (d *Definition) validateRemote() error {
	s := d.Server
	if len(s.Command) > 0 {
		return errors.New("set either server.command or server.url, not both")
	}
	if len(s.Env) > 0 || len(s.EnvPassthrough) > 0 || d.Mode != nil {
		return errors.New("server.env, server.env_passthrough and mode apply to server.command only")
	}
	if err := mcphttp.Validate(s.URL); err != nil {
		return err
	}
	for name, value := range s.Headers {
		lower := strings.ToLower(name)
		if !headerNameRE.MatchString(name) || reservedHeaders[lower] {
			return fmt.Errorf("server.headers: %q is not allowed", name)
		}
		for _, hint := range credentialHeaderHints {
			if strings.Contains(lower, hint) {
				return fmt.Errorf("server.headers: %q looks like a credential header; a credential must not be written into the definition, use server.bearer_secret", name)
			}
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("server.headers: the value of %q contains a control character", name)
		}
	}
	if s.BearerSecret != "" {
		if err := checkEnvName("server.bearer_secret", s.BearerSecret); err != nil {
			return err
		}
	}
	for _, name := range d.Secrets {
		if name != s.BearerSecret {
			return fmt.Errorf("secret %q is not used: a URL server takes only server.bearer_secret", name)
		}
	}
	if s.BearerSecret != "" && len(d.Secrets) == 0 {
		return fmt.Errorf("server.bearer_secret %q must also be declared in secrets", s.BearerSecret)
	}
	return nil
}

// checkNoOverlap refuses a variable that is set from two places (a secret,
// server.env, env_passthrough or mode), so which value the server gets never
// depends on the order in which omac applies them.
func (d *Definition) checkNoOverlap() error {
	from := map[string]string{}
	add := func(kind, name string) error {
		if prev, ok := from[name]; ok {
			return fmt.Errorf("%s %q is also set as %s; set each variable in one place only", kind, name, prev)
		}
		from[name] = kind
		return nil
	}
	for _, name := range d.Secrets {
		if err := add("secret", name); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(d.Server.Env) {
		if err := add("env", name); err != nil {
			return err
		}
	}
	for _, name := range d.Server.EnvPassthrough {
		if err := add("env_passthrough", name); err != nil {
			return err
		}
	}
	if d.Mode != nil {
		return add("mode.name", d.Mode.Name)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
	if def.Server.Remote() {
		return nil
	}
	prog := def.Server.Command[0]
	if !filepath.IsAbs(prog) || workdir == "" {
		return nil
	}
	if insideDir(prog, workdir) {
		return fmt.Errorf("server.command %q is inside the workdir %q, which the agent can modify", prog, workdir)
	}
	return nil
}

// ResolveCommand returns the absolute path of the server's program as the
// host would run it: looked up on PATH for a bare name. It refuses a program
// that resolves into workdir, which happens when PATH names a directory of
// the project (for example node_modules/.bin): the agent could plant the
// program there and have it run outside the sandbox with the secrets.
// Callers run the returned path, so a later PATH lookup cannot pick up a
// program the agent creates afterwards.
func ResolveCommand(def *Definition, workdir string) (string, error) {
	if def.Server.Remote() {
		return "", errors.New("a URL server has no command")
	}
	prog := def.Server.Command[0]
	path, err := exec.LookPath(prog)
	if err != nil {
		return "", fmt.Errorf("server command %q not found: %w", prog, err)
	}
	if !filepath.IsAbs(path) {
		if path, err = filepath.Abs(path); err != nil {
			return "", err
		}
	}
	if workdir != "" && insideDir(path, workdir) {
		return "", fmt.Errorf("server command %q resolves to %q inside the workdir %q, which the agent can modify; use an absolute path outside it", prog, path, workdir)
	}
	return path, nil
}

// insideDir reports whether path is dir or lies below it, both with symlinks
// resolved.
func insideDir(path, dir string) bool {
	rel, err := filepath.Rel(resolveExisting(dir), resolveExisting(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
