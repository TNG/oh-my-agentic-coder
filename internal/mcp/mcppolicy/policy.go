// Package mcppolicy decides what an agent may do with a registered MCP
// server: which JSON-RPC methods and tools are allowed, which need approval,
// and whether the server's tool list still matches what the user reviewed.
// It is pure code with no I/O.
package mcppolicy

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
)

// Class says what a tool may do to the world.
type Class string

const (
	Read   Class = "read"
	Write  Class = "write"
	Delete Class = "delete"
	// Off marks a tool that is reviewed but hidden from the agent and refused.
	Off Class = "off"
)

// Valid reports whether c is one of the four classes.
func (c Class) Valid() bool {
	switch c {
	case Read, Write, Delete, Off:
		return true
	}
	return false
}

// Access limits the classes of tool the agent may call.
type Access string

const (
	ReadOnly        Access = "read-only"
	ReadWrite       Access = "read-write"
	ReadWriteDelete Access = "read-write-delete"
)

// DefaultAccess applies when a definition does not set an access mode.
const DefaultAccess = ReadWriteDelete

// Valid reports whether a is one of the three access modes.
func (a Access) Valid() bool {
	return a == ReadOnly || a == ReadWrite || a == ReadWriteDelete
}

// Allows reports whether the access mode permits calling a tool of class c.
// An unknown access mode or class allows nothing, and Off is never allowed.
func (a Access) Allows(c Class) bool {
	switch a {
	case ReadOnly:
		return c == Read
	case ReadWrite:
		return c == Read || c == Write
	case ReadWriteDelete:
		return c == Read || c == Write || c == Delete
	}
	return false
}

// Tool is the class a definition assigns to one tool.
type Tool struct {
	Name  string `yaml:"name"  json:"name"`
	Class Class  `yaml:"class" json:"class"`
}

// Approvals sets whether a call of each class asks the user first. An empty
// mode means the default for the class.
type Approvals struct {
	Read   approval.Mode `yaml:"read,omitempty"   json:"read,omitempty"`
	Write  approval.Mode `yaml:"write,omitempty"  json:"write,omitempty"`
	Delete approval.Mode `yaml:"delete,omitempty" json:"delete,omitempty"`
}

// DefaultApprovals asks for write and delete and lets read through.
func DefaultApprovals() Approvals {
	return Approvals{Read: approval.None, Write: approval.Required, Delete: approval.Required}
}

// For returns the mode for class c. Anything but a read defaults to
// required, so an unknown class never skips the dialog.
func (a Approvals) For(c Class) approval.Mode {
	d := DefaultApprovals()
	mode, def := d.Delete, d.Delete
	switch c {
	case Read:
		mode, def = a.Read, d.Read
	case Write:
		mode, def = a.Write, d.Write
	case Delete:
		mode = a.Delete
	}
	if mode == "" {
		return def
	}
	return mode
}

// ToolText is the short, sanitized, length-capped text of a tool the user saw
// at review time: its description and a summary of its input parameters. It
// is only there to show what changed later; the hashes decide.
type ToolText struct {
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Params      string `yaml:"params,omitempty"      json:"params,omitempty"`
}

// Review records the tool list the user reviewed: a hash per tool and one over
// the whole list. Text holds the reviewed description of each tool; reviews
// made before it existed have none and still work.
type Review struct {
	ToolsHash string              `yaml:"tools_hash" json:"tools_hash"`
	Tools     map[string]string   `yaml:"tools"      json:"tools"`
	Text      map[string]ToolText `yaml:"text,omitempty" json:"text,omitempty"`
	At        time.Time           `yaml:"at"  json:"at"`
}

// Policy is the effective policy of one registered server.
type Policy struct {
	Server    string
	Access    Access
	Tools     map[string]Class
	Approvals Approvals
	// Review is nil until the user reviews the tool list. A missing review is the
	// gateway's concern (ReasonUnreviewed); Decide does not look at it.
	Review *Review
}

// Call is one parsed JSON-RPC message from the agent.
type Call struct {
	Method string
	// ID is the raw request id, nil for a notification.
	ID json.RawMessage
	// Tool and Args are set for tools/call: params.name and params.arguments.
	Tool string
	Args json.RawMessage
	// Response is true for a message with no method that answers a request
	// the server made.
	Response bool
}

// Decision is the outcome of Decide.
type Decision struct {
	Allow         bool
	NeedsApproval bool
	// Class is the class of the called tool, empty for other messages.
	Class Class
	// Reason is one of the Reason constants when the call is refused.
	Reason string
	// Detail is a human-readable explanation for the agent and the audit log.
	Detail string
}

// Reasons are short stable strings for X-Omac-Reason headers and JSON-RPC
// error messages.
const (
	ReasonUnparseable    = "mcp-unparseable"
	ReasonUnlisted       = "mcp-unlisted"
	ReasonAccess         = "mcp-access"
	ReasonUnreviewed     = "mcp-unreviewed"
	ReasonChanged        = "mcp-changed"
	ReasonUnknownSession = "unknown-session"
)

// Decide applies the method allowlist, the closed tool world and the access
// mode to one message. It never reports a refusal for a message that only
// answers a server request.
func (p *Policy) Decide(c Call) Decision {
	if c.Response {
		return Decision{Allow: true}
	}
	switch {
	case c.Method == "initialize", c.Method == "ping", c.Method == "tools/list",
		strings.HasPrefix(c.Method, "notifications/"):
		return Decision{Allow: true}
	case c.Method == "tools/call":
		return p.decideToolCall(c.Tool)
	}
	return Decision{Reason: ReasonUnlisted, Detail: "method " + c.Method + " is not allowed through the MCP gateway"}
}

func (p *Policy) decideToolCall(tool string) Decision {
	class, ok := p.Tools[tool]
	if !ok {
		return Decision{Reason: ReasonUnlisted, Detail: "tool " + tool + " is not classified for server " + p.Server}
	}
	if class == Off {
		return Decision{Reason: ReasonUnlisted, Class: class, Detail: "tool " + tool + " is turned off for server " + p.Server}
	}
	if !p.Access.Allows(class) {
		return Decision{
			Reason: ReasonAccess, Class: class,
			Detail: "tool " + tool + " is a " + string(class) + " tool and server " + p.Server + " is " + string(p.Access),
		}
	}
	return Decision{Allow: true, Class: class, NeedsApproval: p.Approvals.For(class) == approval.Required}
}

// Visible reports whether the agent may see the tool in tools/list: it is
// classified, not off, and its class is allowed by the access mode.
func (p *Policy) Visible(tool string) bool {
	class, ok := p.Tools[tool]
	return ok && p.Access.Allows(class)
}
