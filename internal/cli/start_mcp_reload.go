package cli

import (
	"context"

	"github.com/TNG/oh-my-agentic-coder/internal/facade"
)

// refresh is a stub until MCP reload is implemented. It returns the
// notices to show the user.
func (p *mcpPlan) refresh(ctx context.Context, f *facade.Facade) []string {
	return nil
}
