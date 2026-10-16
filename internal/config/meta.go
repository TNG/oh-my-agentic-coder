// Package config defines the on-disk configuration formats used by omac:
// skill omac.yaml (with the sidecar block), the per-workdir sidecar.json
// registry, and the oh-my-agentic-coder.yaml launcher config.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/TNG/oh-my-agentic-coder/internal/osinfo"
)

// MetaFileName is the on-disk filename of the per-skill metadata
// document omac reads at register/start time. Originally this was
// "meta.yaml", but that filename collides with the marketplace publishing
// pipeline's own meta.yaml file; renamed to "omac.yaml" to keep the
// two concerns separable. A skill that wants to be both omac-managed
// AND publishable via the marketplace should ship BOTH files.
//
// Always read this constant rather than the literal so a future rename
// stays a one-line change.
const MetaFileName = "omac.yaml"

// Meta is the skill metadata as stored in omac.yaml. Only the fields
// omac cares about are declared. Unknown keys do not stop a skill from
// loading, but LoadMetaWithWarnings reports them, so that `omac register`
// can point out a typo such as `hnit:` instead of silently dropping it.
type Meta struct {
	Name         string   `yaml:"name"`
	Type         string   `yaml:"type"`
	Version      string   `yaml:"version"`
	Description  string   `yaml:"description"`
	Author       string   `yaml:"author"`
	Dependencies []string `yaml:"dependencies"`

	Sidecar *SidecarMeta `yaml:"sidecar,omitempty"`
}

// SidecarMeta is the optional sidecar block in omac.yaml. See
// docs/skills/authoring.md for the full schema.
type SidecarMeta struct {
	Command        []string          `yaml:"command"`
	Mount          string            `yaml:"mount,omitempty"`
	EnvPassthrough []string          `yaml:"env_passthrough,omitempty"`
	Secrets        []SecretSpec      `yaml:"secrets,omitempty"`
	Config         []ConfigSpec      `yaml:"config,omitempty"`
	Health         *HealthSpec       `yaml:"health,omitempty"`
	InstallScripts map[string]string `yaml:"install_scripts,omitempty"`
	Protocols      []string          `yaml:"protocols,omitempty"`
	Limits         *LimitsSpec       `yaml:"limits,omitempty"`
}

// SecretSpec describes a single credential that omac prompts for at
// register time and injects into the sidecar's env at start time.
type SecretSpec struct {
	Name           string `yaml:"name"`
	Description    string `yaml:"description,omitempty"`
	Required       *bool  `yaml:"required,omitempty"` // default true
	Pattern        string `yaml:"pattern,omitempty"`
	DefaultFromEnv string `yaml:"default_from_env,omitempty"`
	Multiline      bool   `yaml:"multiline,omitempty"`
	// Hint is optional multi-line help printed under the description at
	// `omac register`, for example the steps to obtain the value. It is
	// printed again after a value fails the pattern check.
	Hint string `yaml:"hint,omitempty"`
	// Docs optionally points to a file inside the skill that explains the
	// value, with an optional section anchor, e.g.
	// "SKILL.md#obtaining-the-token". An unusable value is a warning at
	// `omac register`, never a load error; see DocsWarnings.
	Docs string `yaml:"docs,omitempty"`
}

// IsRequired returns true unless the spec explicitly opts out.
func (s SecretSpec) IsRequired() bool { return s.Required == nil || *s.Required }

// ValidateValue checks v against the spec's Pattern, if any. A spec without a
// pattern accepts anything.
//
// It lives on the spec rather than in the CLI because three unrelated callers
// need the identical check: `omac register`/`omac secrets` when prompting, and
// internal/skillstate when re-validating a secret supplied from the host
// environment via env_passthrough (keychain values were vetted at register
// time, so that is the only chance to catch a malformed env-supplied one
// before the sidecar receives it).
func (s SecretSpec) ValidateValue(v string) error {
	if s.Pattern == "" {
		return nil
	}
	re, err := regexp.Compile(s.Pattern)
	if err != nil {
		return fmt.Errorf("invalid pattern for %s: %w", s.Name, err)
	}
	if !re.MatchString(v) {
		return fmt.Errorf("value for %s does not match /%s/", s.Name, s.Pattern)
	}
	return nil
}

// ConfigFieldType enumerates the supported value types for non-secret
// skill configuration. Unknown values cause Validate to fail.
type ConfigFieldType string

const (
	ConfigFieldString ConfigFieldType = "string"
	ConfigFieldBool   ConfigFieldType = "bool"
	ConfigFieldInt    ConfigFieldType = "int"
	ConfigFieldEnum   ConfigFieldType = "enum"
)

// ConfigSpec describes one non-secret configuration field that omac
// prompts for at register time. Unlike secrets, the resulting value is
// stored in plain text in <workdir>/.opencode/skill-config.yaml (not
// the OS keychain) and surfaced to the sidecar via the same env-var
// injection mechanism as secrets.
//
// Use ConfigSpec for values that are operationally important but not
// secret: API base URLs, region names, feature flags, retry limits.
// Anything that would be embarrassing in a screenshot belongs in
// `secrets:` instead.
//
// Stored on disk as plain YAML in <workdir>/.opencode/skill-config.yaml.
type ConfigSpec struct {
	Name           string          `yaml:"name"`
	Description    string          `yaml:"description,omitempty"`
	Type           ConfigFieldType `yaml:"type,omitempty"`     // default "string"
	Required       *bool           `yaml:"required,omitempty"` // default true
	Default        string          `yaml:"default,omitempty"`  // pre-fill value at prompt
	DefaultFromEnv string          `yaml:"default_from_env,omitempty"`
	Pattern        string          `yaml:"pattern,omitempty"` // string-only; regex
	Choices        []string        `yaml:"choices,omitempty"` // enum-only; non-empty
	Hint           string          `yaml:"hint,omitempty"`    // multi-line help; see SecretSpec.Hint
	Docs           string          `yaml:"docs,omitempty"`    // file in the skill; see SecretSpec.Docs
}

// IsRequired returns true unless the spec explicitly opts out.
func (c ConfigSpec) IsRequired() bool { return c.Required == nil || *c.Required }

// EffectiveType returns Type, defaulting to "string" if unset.
func (c ConfigSpec) EffectiveType() ConfigFieldType {
	if c.Type == "" {
		return ConfigFieldString
	}
	return c.Type
}

// ValidateValue checks v against the spec's constraints (Choices when
// declared, Pattern for string types). Called both at register time
// (canonicalizeFieldValue in register.go) and at launch time
// (skillstate.resolveConfig) so a stored value that violates the schema is
// refused before it reaches the sidecar's env.
func (c ConfigSpec) ValidateValue(v string) error {
	// Choices constrain the value regardless of declared Type.
	if len(c.Choices) > 0 {
		for _, choice := range c.Choices {
			if v == choice {
				return nil
			}
		}
		return fmt.Errorf("config field %s: value %q is not one of %v", c.Name, v, c.Choices)
	}
	if c.EffectiveType() == ConfigFieldString && c.Pattern != "" {
		re, err := regexp.Compile(c.Pattern)
		if err != nil {
			return fmt.Errorf("config field %s: invalid pattern: %w", c.Name, err)
		}
		if !re.MatchString(v) {
			return fmt.Errorf("config field %s: value %q does not match /%s/", c.Name, v, c.Pattern)
		}
	}
	return nil
}

// HealthSpec controls the liveness probe the supervisor waits on.
type HealthSpec struct {
	Path           string `yaml:"path,omitempty"`
	InitialDelayMS int    `yaml:"initial_delay_ms,omitempty"`
	TimeoutMS      int    `yaml:"timeout_ms,omitempty"`
	IntervalMS     int    `yaml:"interval_ms,omitempty"`
}

// Defaults fills zero values with the documented defaults and returns a copy.
func (h *HealthSpec) Defaults() HealthSpec {
	out := HealthSpec{}
	if h != nil {
		out = *h
	}
	if out.Path == "" {
		out.Path = "/status"
	}
	if out.InitialDelayMS == 0 {
		out.InitialDelayMS = 200
	}
	if out.TimeoutMS == 0 {
		out.TimeoutMS = 5000
	}
	if out.IntervalMS == 0 {
		out.IntervalMS = 500
	}
	return out
}

// LimitsSpec configures per-skill proxy limits.
type LimitsSpec struct {
	MaxBodyBytes    int64 `yaml:"max_body_bytes,omitempty"`
	IdleTimeoutSecs int   `yaml:"idle_timeout_secs,omitempty"`
}

var (
	envNameRE   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	mountRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	skillNameRE = mountRE // skill names share the mount grammar
)

// ValidSkillName returns an error when name is not a valid skill name.
// Valid names match ^[a-z0-9][a-z0-9-]*$, must not be "." or "..", and must
// not contain any path separator. This is enforced at every name→path sink
// (approval store, snapshot dir, facade route, keychain service) so that
// agent-supplied names cannot escape the store or collide with other skills.
func ValidSkillName(name string) error {
	if name == "" {
		return fmt.Errorf("skill name must not be empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("skill name %q is a reserved path element", name)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("skill name %q must not contain a path separator", name)
	}
	if !skillNameRE.MatchString(name) {
		return fmt.Errorf("skill name %q must match %s", name, skillNameRE.String())
	}
	return nil
}

// LoadMeta reads omac.yaml from path and validates it. Keys omac does not
// know are ignored; use LoadMetaWithWarnings to list them.
func LoadMeta(path string) (*Meta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read meta: %w", err)
	}
	var m Meta
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse meta: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadMetaWithWarnings is LoadMeta plus one warning per key in the file
// that omac does not know, e.g.
// "sidecar.secrets[0].hnit: unknown field (ignored)". The keys are
// ignored exactly as LoadMeta ignores them; the warnings are only for
// showing to the skill author.
//
// The warnings are also returned when decoding or validation fails,
// because an unknown key often explains the failure: with `- nmae: TOKEN`
// the error is "name is not a valid env var name", and the warning names
// the typo. Only a read or YAML syntax error returns no warnings.
func LoadMetaWithWarnings(path string) (*Meta, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read meta: %w", err)
	}
	// Parse once into a node tree, then decode the Meta from it and walk
	// the same tree for unknown keys.
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse meta: %w", err)
	}
	var m Meta
	var warnings []string
	if len(doc.Content) > 0 {
		// Collect the unknown keys first, so they are also reported when
		// decoding fails on a value of the wrong type.
		for _, key := range unknownMetaKeys(doc.Content[0]) {
			warnings = append(warnings, key+": unknown field (ignored)")
		}
		if err := doc.Decode(&m); err != nil {
			return nil, warnings, fmt.Errorf("parse meta: %w", err)
		}
	}
	if err := m.Validate(); err != nil {
		return nil, warnings, err
	}
	return &m, warnings, nil
}

// unknownMetaKeys lists the keys in a parsed omac.yaml document that do
// not map to a field of Meta, as dotted paths with list indexes, e.g.
// "sidecar.secrets[0].hnit". Map-typed fields such as install_scripts
// accept any key and are not checked.
//
// yaml.v3's Decoder.KnownFields(true) finds the same keys, but reports
// them as decode errors with a Go type name, e.g. "line 7: field hnit not
// found in type config.SecretSpec". Walking the parsed YAML tree next to
// the Go type gives the path a skill author knows from the docs instead.
func unknownMetaKeys(root *yaml.Node) []string {
	var out []string
	collectUnknownKeys(root, reflect.TypeOf(Meta{}), "", &out)
	return out
}

// collectUnknownKeys walks node, which should hold a value of type t, and
// appends the path of every mapping key that t has no field for.
func collectUnknownKeys(node *yaml.Node, t reflect.Type, path string, out *[]string) {
	if node == nil {
		return
	}
	if node.Kind == yaml.AliasNode {
		collectUnknownKeys(node.Alias, t, path, out)
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return
		}
		fields := yamlFieldTypes(t)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i], node.Content[i+1]
			// "<<: *anchor" merges another mapping into this one; check
			// the merged keys as if they were written here.
			if key.Value == "<<" && key.Tag == "!!merge" {
				if val.Kind == yaml.SequenceNode {
					for _, m := range val.Content {
						collectUnknownKeys(m, t, path, out)
					}
				} else {
					collectUnknownKeys(val, t, path, out)
				}
				continue
			}
			p := key.Value
			if path != "" {
				p = path + "." + key.Value
			}
			ft, ok := fields[key.Value]
			if !ok {
				*out = append(*out, p)
				continue
			}
			collectUnknownKeys(val, ft, p, out)
		}
	case reflect.Slice:
		if node.Kind != yaml.SequenceNode {
			return
		}
		for i, item := range node.Content {
			collectUnknownKeys(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

// yamlFieldTypes maps each YAML key of struct type t to its field type,
// following the `yaml:"name,..."` tags the way yaml.v3 does: a field
// without a name in its tag uses its lowercased Go name, "-" is skipped,
// and ",inline" pulls in the fields of an embedded struct.
func yamlFieldTypes(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if strings.Contains(","+opts+",", ",inline,") {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range yamlFieldTypes(ft) {
					out[k] = v
				}
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = f.Type
	}
	return out
}

// Validate checks the invariants of a Meta value (including the sidecar block).
func (m *Meta) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("%s: name is required", MetaFileName)
	}
	if m.Sidecar != nil {
		if err := m.Sidecar.Validate(m.Name); err != nil {
			return err
		}
	}
	return nil
}

// Validate enforces the sidecar-block schema.
func (s *SidecarMeta) Validate(skillName string) error {
	if len(s.Command) == 0 {
		return fmt.Errorf("sidecar.command is required")
	}
	if s.Mount != "" && !mountRE.MatchString(s.Mount) {
		return fmt.Errorf("sidecar.mount %q must match %s", s.Mount, mountRE.String())
	}

	// Track every authoritatively-declared env var (secrets + config) so
	// we can reject collisions between them. Both write to the same env
	// namespace at sidecar spawn time and would race in supervisor.go's
	// vars-map construction, depending on map iteration order.
	//
	// env_passthrough is intentionally NOT included in this check: the
	// existing convention (see echo-rest) is to also list a secret in
	// env_passthrough as a fallback for environments where the keychain
	// is unavailable (sandboxed CI runners, headless servers). At runtime
	// secrets/config win over passthrough deterministically, so the
	// duplicate is harmless.
	declared := map[string]string{}
	claim := func(name, kind string) error {
		if other, ok := declared[name]; ok {
			return fmt.Errorf("sidecar: env var %q declared by both %s and %s; pick one", name, other, kind)
		}
		declared[name] = kind
		return nil
	}

	for i, sec := range s.Secrets {
		if !envNameRE.MatchString(sec.Name) {
			return fmt.Errorf("sidecar.secrets[%d].name %q is not a valid env var name", i, sec.Name)
		}
		if sec.Pattern != "" {
			if _, err := regexp.Compile(sec.Pattern); err != nil {
				return fmt.Errorf("sidecar.secrets[%d].pattern is not a valid regex: %w", i, err)
			}
		}
		if err := claim(sec.Name, "secrets"); err != nil {
			return err
		}
	}

	for i, c := range s.Config {
		if !envNameRE.MatchString(c.Name) {
			return fmt.Errorf("sidecar.config[%d].name %q is not a valid env var name", i, c.Name)
		}
		switch c.EffectiveType() {
		case ConfigFieldString:
			if c.Pattern != "" {
				if _, err := regexp.Compile(c.Pattern); err != nil {
					return fmt.Errorf("sidecar.config[%d].pattern is not a valid regex: %w", i, err)
				}
			}
			if len(c.Choices) > 0 {
				return fmt.Errorf("sidecar.config[%d]: 'choices' is only valid with type=enum", i)
			}
		case ConfigFieldBool:
			if c.Pattern != "" || len(c.Choices) > 0 {
				return fmt.Errorf("sidecar.config[%d]: type=bool does not accept pattern/choices", i)
			}
			if c.Default != "" {
				if _, err := parseBoolField(c.Default); err != nil {
					return fmt.Errorf("sidecar.config[%d].default %q is not a valid bool", i, c.Default)
				}
			}
		case ConfigFieldInt:
			if c.Pattern != "" || len(c.Choices) > 0 {
				return fmt.Errorf("sidecar.config[%d]: type=int does not accept pattern/choices", i)
			}
			if c.Default != "" {
				if _, err := strconv.ParseInt(c.Default, 10, 64); err != nil {
					return fmt.Errorf("sidecar.config[%d].default %q is not a valid integer", i, c.Default)
				}
			}
		case ConfigFieldEnum:
			if len(c.Choices) == 0 {
				return fmt.Errorf("sidecar.config[%d]: type=enum requires non-empty 'choices'", i)
			}
			if c.Pattern != "" {
				return fmt.Errorf("sidecar.config[%d]: type=enum does not accept 'pattern'", i)
			}
			if c.Default != "" {
				ok := false
				for _, choice := range c.Choices {
					if choice == c.Default {
						ok = true
						break
					}
				}
				if !ok {
					return fmt.Errorf("sidecar.config[%d].default %q is not in choices %v", i, c.Default, c.Choices)
				}
			}
		default:
			return fmt.Errorf("sidecar.config[%d].type %q is not one of: string, bool, int, enum", i, c.Type)
		}
		if err := claim(c.Name, "config"); err != nil {
			return err
		}
	}

	for _, p := range s.EnvPassthrough {
		if !envNameRE.MatchString(p) {
			return fmt.Errorf("sidecar.env_passthrough entry %q is not a valid env var name", p)
		}
		// env_passthrough is allowed to overlap with a secret (legacy
		// fallback pattern) but not with a config field — fields aren't
		// secret enough to have a fallback semantics, and the duplicate
		// would just be confusing to skill authors.
		if other, ok := declared[p]; ok && other == "config" {
			return fmt.Errorf("sidecar: env var %q declared by both env_passthrough and config; pick one", p)
		}
	}
	if s.Health != nil && s.Health.Path != "" {
		p := s.Health.Path
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "@:") {
			return fmt.Errorf("sidecar.health.path %q must start with '/' and contain no '@' or ':'", p)
		}
	}
	return nil
}

// splitDocsRef splits a `docs` value into the file path and the optional
// section anchor after the first '#'. For "SKILL.md#getting-a-token" it
// returns "SKILL.md" and "getting-a-token".
func splitDocsRef(ref string) (path, anchor string) {
	path, anchor, _ = strings.Cut(ref, "#")
	return path, anchor
}

// docsRefProblem says why a `docs` value cannot be shown, or returns ""
// when it can. The path must name a file inside the skill directory:
// relative, and without ".." steps that leave it. "docs/token.md#steps"
// is fine; "/etc/passwd", "../other/SKILL.md", "~/notes.md",
// "https://example.com/help" and "#steps" (no file) are not.
//
// This is deliberately not part of Validate. `docs` is only help text, and
// before omac knew the key, a skill with any `docs` value loaded fine; a
// bad value must not make such a skill fail to start. `omac register`
// reports it as a warning (see DocsWarnings) and shows no "More:" line.
func docsRefProblem(ref string) string {
	path, _ := splitDocsRef(ref)
	switch {
	case path == "":
		return fmt.Sprintf("%q does not name a file; use a path inside the skill, e.g. SKILL.md#section", ref)
	case strings.Contains(path, "://"):
		return fmt.Sprintf("%q must be a file path inside the skill, not a URL", ref)
	case strings.HasPrefix(path, "~"):
		return fmt.Sprintf("%q must be a path relative to the skill directory; '~' is not expanded", ref)
	case filepath.IsAbs(path) || strings.HasPrefix(path, "/"):
		return fmt.Sprintf("%q must be a path relative to the skill directory, not an absolute path", ref)
	case !filepath.IsLocal(path):
		return fmt.Sprintf("%q must stay inside the skill directory (no '..' that leaves it)", ref)
	}
	return ""
}

// DocsPath returns the absolute path of a `docs` value inside skillDir,
// plus its section anchor. It returns "" when ref is empty or unusable
// (see docsRefProblem). It does not check that the file exists.
func DocsPath(skillDir, ref string) (abs, anchor string) {
	if ref == "" || docsRefProblem(ref) != "" {
		return "", ""
	}
	path, anchor := splitDocsRef(ref)
	dir, err := filepath.Abs(skillDir)
	if err != nil {
		dir = skillDir
	}
	return filepath.Join(dir, path), anchor
}

// DocsWarnings returns one message per `docs` entry that omac cannot show,
// e.g. "sidecar.secrets[0].docs: SKILL.md not found in the skill
// directory". Reasons: the value is not a path inside the skill (see
// docsRefProblem), the file does not exist, or it is a directory. A bad
// docs value only loses some help text, so callers report these as
// warnings rather than refusing the skill.
func (m *Meta) DocsWarnings(skillDir string) []string {
	if m.Sidecar == nil {
		return nil
	}
	var out []string
	check := func(field, ref string) {
		if ref == "" {
			return
		}
		if problem := docsRefProblem(ref); problem != "" {
			out = append(out, field+": "+problem)
			return
		}
		abs, _ := DocsPath(skillDir, ref)
		path, _ := splitDocsRef(ref)
		fi, err := os.Stat(abs)
		switch {
		case err != nil:
			out = append(out, fmt.Sprintf("%s: %s not found in the skill directory", field, path))
		case fi.IsDir():
			out = append(out, fmt.Sprintf("%s: %s is a directory, not a file", field, path))
		}
	}
	for i, s := range m.Sidecar.Secrets {
		check(fmt.Sprintf("sidecar.secrets[%d].docs", i), s.Docs)
	}
	for i, c := range m.Sidecar.Config {
		check(fmt.Sprintf("sidecar.config[%d].docs", i), c.Docs)
	}
	return out
}

// PatternlessSecretStringFields returns the string config fields that carry
// neither a pattern nor choices on a skill that also declares secrets. Such a
// field accepts any value from the agent-writable config store, which the
// sidecar then receives alongside its secrets, so registration warns about it.
func (s *SidecarMeta) PatternlessSecretStringFields() []string {
	if len(s.Secrets) == 0 {
		return nil
	}
	var out []string
	for _, c := range s.Config {
		if c.EffectiveType() == ConfigFieldString && c.Pattern == "" && len(c.Choices) == 0 {
			out = append(out, c.Name)
		}
	}
	return out
}

// PatternlessSecretStringFields is Meta's convenience wrapper over
// SidecarMeta.PatternlessSecretStringFields; nil when there is no sidecar.
func (m *Meta) PatternlessSecretStringFields() []string {
	if m.Sidecar == nil {
		return nil
	}
	return m.Sidecar.PatternlessSecretStringFields()
}

// parseBoolField accepts a small set of human-friendly bool spellings.
// Used both for validating ConfigSpec.Default and for converting prompt
// input. Returns the canonical value ("true" or "false") on success.
func parseBoolField(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "t", "yes", "y", "1", "on":
		return "true", nil
	case "false", "f", "no", "n", "0", "off":
		return "false", nil
	default:
		return "", fmt.Errorf("not a bool: %q (try true/false, yes/no, 1/0)", s)
	}
}

// ParseBoolField is the exported helper for callers outside this package
// (CLI prompt handler). See parseBoolField for accepted spellings.
func ParseBoolField(s string) (string, error) { return parseBoolField(s) }

// MountOrDefault returns the routing prefix for this skill.
func (s *SidecarMeta) MountOrDefault(skillName string) string {
	if s.Mount != "" {
		return s.Mount
	}
	return skillName
}

// InstallScriptFor returns the script path for the given OS (possibly empty).
func (s *SidecarMeta) InstallScriptFor(o osinfo.OS) string {
	if s.InstallScripts == nil {
		return ""
	}
	return s.InstallScripts[string(o)]
}

// BundleHash returns a sha256 digest covering every meaningful file in
// a skill directory: omac.yaml, the sidecar entry-point, helper
// modules, install scripts. Runtime artifacts and developer caches
// (virtualenvs, *.pyc, node_modules, .git, .DS_Store, ...) are
// excluded so the hash stays stable across `pip install` runs and the
// like.
//
// Wire format (a Merkle-style two-level hash):
//
//	for each included file (sorted by repo-relative path):
//	    digest_input += relative_path + "\x00" + sha256(file_bytes) + "\x00"
//	bundle_hash = "sha256:" + hex(sha256(digest_input))
//
// The NUL byte is a separator that no path or hex digest can contain,
// so file boundaries are unambiguous. Sorting by relative path makes
// the result deterministic regardless of filesystem traversal order.
//
// Returns an error only if a directory entry can't be stat'd or read;
// missing dir is reported as a regular fs.PathError.
//
// The skill root is resolved via filepath.EvalSymlinks before walking.
// The root is commonly a symlink — either the skills directory itself
// (~/.config/opencode/skills -> /repo/opencode/skills/) or an individual
// skill within it (skills/foo -> ../../library/skills/foo/) — so the tree
// stays git-versioned in a separate repo. Without resolution, WalkDir
// Lstats the root, sees ModeSymlink, fires the callback once with
// rel == "." (skipped below), and never descends — yielding the empty
// sha256:e3b0c44… digest. On any error (including a not-exist target),
// the walk proceeds against the unresolved abs so the missing-dir
// contract still gets its fs.PathError from the walk, not from
// EvalSymlinks. In-tree symlinks remain skipped per the per-entry rule
// below.
func BundleHash(skillDir string) (string, error) {
	abs, err := filepath.Abs(skillDir)
	if err != nil {
		return "", fmt.Errorf("bundle hash: abs: %w", err)
	}
	root := abs
	if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		root = resolved
	}
	type item struct {
		rel string
		sum [sha256.Size]byte
	}
	var items []item

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		// Normalize separator so a Windows-style backslash never sneaks
		// into the digest (we don't currently support Windows builds,
		// but cheap insurance for the day someone tries).
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if isExcludedDirName(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// omac.yaml being a non-regular file (e.g. a symlink into an
			// excluded directory) means the executed manifest is outside the
			// hash — the entire skill's approval is meaningless. Fail loudly
			// so the caller cannot unknowingly approve an unhashable skill.
			if d.Name() == MetaFileName {
				return fmt.Errorf("bundle hash: %s is not a regular file: approval would not cover the executed manifest", MetaFileName)
			}
			// Other non-regular files (symlinks, sockets, devices) are skipped.
			// Hashing through symlinks would let a target replacement silently
			// change the bundle without tripping detection.
			return nil
		}
		if isExcludedFileName(d.Name()) {
			return nil
		}
		raw, readErr := os.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("bundle hash: read %s: %w", rel, readErr)
		}
		items = append(items, item{rel: rel, sum: sha256.Sum256(raw)})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })

	h := sha256.New()
	for _, it := range items {
		h.Write([]byte(it.rel))
		h.Write([]byte{0})
		h.Write(it.sum[:])
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// IsExcludedDirName reports whether walking should skip a subtree
// matching a known runtime-artifact / VCS / cache directory name.
// The check is on the basename only, so a skill that legitimately
// contains a file called "build" (in some content sense) is still
// hashed; only directory matches trigger a skip.
//
// Exported so snapshot.go can apply the same exclusions, keeping the
// hashed set identical to the executed set.
func IsExcludedDirName(name string) bool {
	return isExcludedDirName(name)
}

func isExcludedDirName(name string) bool {
	switch name {
	case ".git", ".hg", ".svn",
		".venv", "venv", ".tox", "__pycache__",
		"node_modules",
		".cache", ".pytest_cache", ".mypy_cache", ".ruff_cache",
		"target", "dist", "build",
		".idea", ".vscode":
		return true
	}
	return false
}

// isExcludedFileName reports whether a file should be skipped from
// the hash. Compiled output and OS-level junk only.
func isExcludedFileName(name string) bool {
	switch name {
	case ".DS_Store", "Thumbs.db", ".gitignore.swp":
		return true
	}
	// Suffix-based: Python bytecode, common editor swap files.
	for _, suffix := range []string{".pyc", ".pyo", ".swp", "~"} {
		if strings.HasSuffix(name, suffix) && name != suffix {
			return true
		}
	}
	return false
}
