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
		Endpoints: []config.ApprovalSpec{{Match: "GET /status", Approval: approval.None}},
	}}}
	set := routeApprovals(m)
	if _, ok := set.Lookup("GET", []string{"status"}); !ok {
		t.Error("declared endpoint not matched")
	}
	if _, ok := set.Lookup("GET", []string{"other"}); ok {
		t.Error("undeclared endpoint matched")
	}
	// A declaration that cannot compile admits nothing, not even the
	// entries in it that are valid on their own.
	bad := &config.Meta{Sidecar: &config.SidecarMeta{Approvals: &config.ApprovalsSpec{
		Endpoints: []config.ApprovalSpec{
			{Match: "GET /status", Approval: approval.None},
			{Match: "/broken", Approval: approval.None},
		},
	}}}
	if e, ok := routeApprovals(bad).Lookup("GET", []string{"status"}); ok {
		t.Errorf("broken declaration admitted GET /status via %q", e.Match)
	}
}
