// Package manifest renders the skills manifest text injected into an agent's
// context at SessionStart. It is harness-agnostic: the caller passes the raw
// activate-response JSON and the active harness's skills directory, and
// Render returns the markdown block. Each bridge wraps it in its
// harness-specific JSON envelope.
package manifest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// skill mirrors the fields from the activate response's skills array.
type skill struct {
	Name    string   `json:"name"`
	Scope   string   `json:"scope"`
	State   string   `json:"state"`
	Base    string   `json:"base"`
	Missing []string `json:"missing"`
	Detail  string   `json:"detail"`
}

// activateResponse mirrors the omac control plane's activate response.
type activateResponse struct {
	Dir    string  `json:"dir"`
	State  string  `json:"state"`
	Skills []skill `json:"skills"`
}

// Render returns the manifest text injected into an agent's context at
// SessionStart. activateJSON is the raw JSON body from POST /__omac__/activate.
// skillsDir is the active harness's workdir skills directory (e.g.
// ".claude/skills", ".codex/skills", ".copilot/skills"). The output is a
// markdown block; each bridge wraps it in its harness-specific envelope.
func Render(activateJSON, skillsDir string) string {
	var resp activateResponse
	if err := json.Unmarshal([]byte(activateJSON), &resp); err != nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("## omac skills available in this workspace\n\n")
	b.WriteString("You can call the following skill HTTP endpoints. Each `base` is the root URL for that skill's sidecar; append the skill's documented path.\n\n")
	b.WriteString("This workspace's project directory is: `")
	b.WriteString(resp.Dir)
	b.WriteString("`\n")

	// Global skill target_path warning
	hasGlobalReady := false
	for _, sk := range resp.Skills {
		if sk.Scope == "global" && sk.State == "ready" {
			hasGlobalReady = true
			break
		}
	}
	if hasGlobalReady {
		b.WriteString("\nIMPORTANT: **global** skills are shared by every workspace. When a global skill writes into the project (e.g. the marketplace installing a skill), you MUST pass this workspace's project directory explicitly — for the marketplace use `\"target_path\": \"")
		b.WriteString(resp.Dir)
		b.WriteString("/")
		b.WriteString(skillsDir)
		b.WriteString("\"` (the active harness's skills directory) in the /install request body. Otherwise it installs into the wrong directory.\n")
	}

	b.WriteString("\n")

	// Sort skills by name
	sorted := make([]skill, len(resp.Skills))
	copy(sorted, resp.Skills)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})

	for _, sk := range sorted {
		switch sk.State {
		case "ready":
			if sk.Base != "" {
				fmt.Fprintf(&b, "- **%s** (%s) — ready — base: `%s`\n", sk.Name, sk.Scope, sk.Base)
			}
		case "pending-credentials":
			missing := strings.Join(sk.Missing, ", ")
			fmt.Fprintf(&b, "- **%s** (%s) — UNAVAILABLE (missing credentials: %s). Run in your own terminal: %s\n",
				sk.Name, sk.Scope, missing, secretsHint(sk.Name, sk.Missing))
		case "broken":
			detail := sk.Detail
			if detail == "" {
				detail = "see omac logs"
			}
			fmt.Fprintf(&b, "- **%s** (%s) — BROKEN: %s\n", sk.Name, sk.Scope, detail)
		}
	}

	return b.String()
}

// secretsHint builds the "omac secrets set" commands for missing credentials.
func secretsHint(skillName string, missing []string) string {
	var parts []string
	for _, m := range missing {
		parts = append(parts, "omac secrets set "+skillName+" "+m)
	}
	return strings.Join(parts, " ; ")
}
