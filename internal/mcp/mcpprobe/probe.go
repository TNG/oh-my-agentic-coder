// Package mcpprobe starts a registered MCP server on the host and lists its tools.
package mcpprobe

import (
	"context"
	"errors"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

// ListTools is a stub until the probe is implemented. The string result is
// the tail of the server's stderr.
func ListTools(ctx context.Context, e *mcpreg.Entry, sec map[string]secrets.Secret, baseEnv []string, timeout time.Duration) ([]mcppolicy.ToolInfo, string, error) {
	return nil, "", errors.New("not implemented")
}
