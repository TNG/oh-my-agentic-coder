// Package mcpinject decides how registered MCP servers reach a harness and
// what to do about MCP servers the harness already knows.
package mcpinject

// Mode is how unregistered MCP servers found in harness config are handled.
type Mode string

const (
	ModeAllow  Mode = "allow"
	ModeWarn   Mode = "warn"
	ModeRefuse Mode = "refuse"
)

// Valid reports whether m is one of the defined modes.
func (m Mode) Valid() bool {
	switch m {
	case ModeAllow, ModeWarn, ModeRefuse:
		return true
	}
	return false
}

// Policy is the resolved handling of existing MCP config. Active is false
// when nothing is registered and no mode was set, so omac leaves MCP alone.
type Policy struct {
	Unregistered Mode
	Active       bool
}

// Existing is an MCP server a harness config file already defines.
type Existing struct {
	Harness string
	File    string
	Name    string
	// UserLevel is set for a file that applies to every project of the user.
	UserLevel bool
	// Managed is set for a file an administrator deploys; the user cannot remove its entries.
	Managed bool
	// PluginProvided is set for a server a plugin brings along.
	PluginProvided bool
	// Transport is the server's transport as the config declares it, such as stdio or http.
	Transport string
	Command   string
	Args      []string
	// EnvKeys and HeaderKeys hold names only; values are never stored.
	EnvKeys    []string
	HeaderKeys []string
	// AlsoRegistered is set by the caller when an omac registry entry has the same name.
	AlsoRegistered bool
}

// Finding kinds. Either may hide servers omac cannot see.
const (
	FindingParseError = "parse-error"
	FindingUnreadable = "unreadable"
)

// Finding is a harness config file omac could not read or parse.
type Finding struct {
	File   string
	Kind   string
	Detail string
	// Managed is set for a file an administrator deploys.
	Managed bool
	// PluginProvided is set for a file a plugin brings along.
	PluginProvided bool
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
