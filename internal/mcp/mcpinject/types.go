// Package mcpinject decides how registered MCP servers reach a harness and
// what to do about MCP servers the harness already knows.
package mcpinject

// Mode is how unregistered MCP servers found in harness config are handled.
type Mode string

const (
	ModeAllow  Mode = "allow"
	ModeWarn   Mode = "warn"
	ModeIgnore Mode = "ignore"
	ModeRefuse Mode = "refuse"
)

// Valid reports whether m is one of the defined modes.
func (m Mode) Valid() bool {
	switch m {
	case ModeAllow, ModeWarn, ModeIgnore, ModeRefuse:
		return true
	}
	return false
}

// CredMode is how literal credentials in harness MCP config are handled.
type CredMode string

const (
	CredAllow  CredMode = "allow"
	CredWarn   CredMode = "warn"
	CredRefuse CredMode = "refuse"
)

// Valid reports whether c is one of the defined modes.
func (c CredMode) Valid() bool {
	switch c {
	case CredAllow, CredWarn, CredRefuse:
		return true
	}
	return false
}

// Policy is the resolved handling of existing MCP config. Active is false
// when nothing is registered and no mode was set, so omac leaves MCP alone.
type Policy struct {
	Unregistered       Mode
	LiteralCredentials CredMode
	Active             bool
}

// Existing is an MCP server a harness config file already defines.
type Existing struct {
	Harness string
	File    string
	Name    string
}

// Finding is a literal credential found in an existing MCP definition.
type Finding struct {
	File   string
	Server string
	Field  string
	Kind   string
}

// Server is a registered server as the harness is told to start it.
type Server struct {
	Name string
	Argv []string
	Env  map[string]string
}

// Injection is what the launch adds so the harness sees the servers.
type Injection struct {
	Env       map[string]string
	AllowEnv  []string
	InnerArgs []string
}
