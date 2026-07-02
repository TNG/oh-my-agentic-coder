package cli

import "testing"

func TestProvenanceViewJSONRoundTrip(t *testing.T) {
	v := provenanceView{
		Profile: profileSource{Name: "default", Path: "/x/default.json", Source: "global"},
		Network: networkView{
			Mode:          "filtered",
			PromptOn:      true,
			OnUnavailable: "deny",
			Entries: []provEntry{
				{Entry: "github.com", Action: "allow", Source: "workdir"},
				{Entry: "evil.com", Action: "deny", Source: "global"},
			},
		},
		Filesystem: filesystemView{
			WorkdirAccess: "readwrite",
			Entries: []provEntry{
				{Entry: "~/.cache", Action: "allow", Source: "builtin"},
			},
		},
		Environment: environmentView{
			Entries: []provEntry{
				{Entry: "LD_*", Action: "deny", Source: "blocklist"},
			},
		},
		Skills: skillsView{
			Workdir: "/home/user/proj",
			Entries: []provEntry{
				{Entry: "slack", Action: "registered", Source: "workdir"},
			},
		},
	}
	if v.Network.Entries[0].Entry != "github.com" {
		t.Fatal("entry mismatch")
	}
	if v.Skills.Workdir != "/home/user/proj" {
		t.Fatal("workdir mismatch")
	}
}
