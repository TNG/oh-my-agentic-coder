package cli

import (
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

func TestRouteApprovals(t *testing.T) {
	if routeApprovals(nil) != nil || routeApprovals(&config.Meta{}) != nil ||
		routeApprovals(&config.Meta{Sidecar: &config.SidecarMeta{}}) != nil {
		t.Error("a skill without an approvals block must be ungated")
	}
	m := &config.Meta{Sidecar: &config.SidecarMeta{Approvals: &config.ApprovalsSpec{
		Version:   1,
		Endpoints: []config.ApprovalSpec{{Match: "GET /status", Approval: approval.None}},
	}}}
	set := routeApprovals(m)
	if _, ok := set.Lookup("GET", []string{"status"}); !ok {
		t.Error("declared endpoint not matched")
	}
	if _, ok := set.Lookup("GET", []string{"other"}); ok {
		t.Error("undeclared endpoint matched")
	}
	// A declaration that cannot compile admits nothing.
	bad := &config.Meta{Sidecar: &config.SidecarMeta{Approvals: &config.ApprovalsSpec{Version: 9}}}
	if _, ok := routeApprovals(bad).Lookup("GET", nil); ok {
		t.Error("broken declaration admitted a request")
	}
}
