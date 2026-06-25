package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Harness describes an inner agentic-coder harness that omac can launch
// inside the sandbox. The Go core is harness-agnostic — it execs whatever
// inner command resolves — so all harness-specific knowledge lives here as
// data. Adding support for a new agentic coder is a matter of appending one
// Harness descriptor to the registry (plus shipping its client-side bridge
// assets); no command-dispatch or launch call site needs to change.
//
// See openspec/changes/support-claude-code-harness and oh-my-agentic-coder.md
// §4/§17.
type Harness struct {
	// Name is the canonical, lowercase harness identifier (e.g. "opencode",
	// "claude-code"). It is what `omac start <name>` matches after alias
	// resolution.
	Name string

	// Aliases are additional accepted spellings for Name (e.g. "claude" for
	// "claude-code", "oc" for "opencode"). They MUST be unique across the
	// whole registry.
	Aliases []string

	// InnerCmd is the default inner command argv used when neither a sandbox
	// profile nor an explicit --inner override supplies one. The first
	// element is the executable; the rest are default arguments.
	InnerCmd []string

	// ServerLaunch describes how `omac serve` turns the inner command into a
	// long-lived server. A nil/empty ServerLaunch means the harness has no
	// distinct server mode and runs the inner command as-is under serve.
	ServerLaunch *ServerLaunch

	// BridgeDir is the project-relative directory where this harness expects
	// its client-side bridge assets to live (e.g. ".opencode/plugins" for
	// OpenCode, ".claude" for Claude Code). Informational: omac does not
	// install bridge assets itself, but discovery and docs reference it.
	BridgeDir string

	// SkillsBase is the directory base name this harness owns for skills,
	// matching where the harness's own loader reads SKILL.md. omac scans
	// "<base>/skills" under workdir and user-global roots. OpenCode owns
	// "opencode" (.opencode/skills, ~/.config/opencode/skills); Claude Code
	// owns "claude" (.claude/skills, ~/.claude/skills). The shared neutral
	// base SharedSkillsBase ("agents") is scanned by every harness in
	// addition to the active harness's own base. Skill discovery NEVER scans
	// another harness's own base — see internal/skillsource.
	SkillsBase string

	// UserConfigHome, when non-empty, is the harness's user-global config
	// directory as a $HOME-relative path (e.g. ".claude" for Claude Code,
	// whose config home is ~/.claude rather than ~/.config/claude). When
	// empty, the harness follows the XDG convention and its user config dir is
	// <userConfigRoot>/<SkillsBase> (i.e. ~/.config/<base>, honoring
	// $XDG_CONFIG_HOME). GlobalSkillsDir derives "<config home>/skills" from
	// this — the directory the harness's own loader reads global skills from.
	UserConfigHome string

	// Session, when non-nil, declares how omac re-enters prior sessions of
	// this harness for `omac continue` and `omac resume`. A nil Session means
	// the harness exposes no session continue/resume support, and those
	// subcommands report it as unsupported. See HarnessSession.
	Session *HarnessSession
}

// SessionListKind selects how omac enumerates a harness's prior sessions for
// `omac resume`. The actual listing logic lives in the session package (so
// config stays free of CLI/filesystem dependencies); this enum is the data
// that keys it.
type SessionListKind int

const (
	// SessionListNone means the harness has no way to list sessions; `omac
	// resume` reports listing unsupported (continue may still work).
	SessionListNone SessionListKind = iota
	// SessionListOpenCodeCLI lists via `opencode session list --format json`.
	SessionListOpenCodeCLI
	// SessionListClaudeFiles lists by reading Claude Code's per-project
	// session store under ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl.
	SessionListClaudeFiles
)

// HarnessSession encodes the harness-specific knowledge `omac continue` and
// `omac resume` need: the inner flags that re-enter sessions, and how to
// enumerate prior sessions for the picker. It is pure data so the harness
// registry stays declarative (the I/O lives in the session package, keyed on
// ListKind).
type HarnessSession struct {
	// ContinueArgs are appended to the inner command to continue the most
	// recent session for the current workdir (opencode/claude: ["--continue"]).
	ContinueArgs []string

	// ResumeByIDArgs builds the inner args that resume a specific session id
	// (opencode: ["--session", id]; claude: ["--resume", id]).
	ResumeByIDArgs func(id string) []string

	// ListKind selects the strategy `omac resume` uses to enumerate sessions.
	ListKind SessionListKind
}

// SharedSkillsBase is the neutral, harness-independent skills directory base
// (".agents/skills", "~/.config/agents/skills", "~/.agents/skills"). It is in
// scope for every harness, so a skill placed there is visible regardless of
// which inner harness is running.
const SharedSkillsBase = "agents"

// ServerLaunch encodes the convention by which a harness's inner command is
// turned into a long-lived server under `omac serve`.
//
// The only convention in use today is "inject a fixed subcommand right after
// the executable if the caller did not already supply one" (OpenCode's
// `serve`). The struct is deliberately small and data-driven so a future
// harness can declare its own subcommand without new branching logic.
type ServerLaunch struct {
	// Subcommand, when non-empty, is inserted immediately after the inner
	// executable if neither the inner command tail nor the trailing args
	// already begin with a subcommand (a non-flag positional). For OpenCode
	// this is "serve".
	Subcommand string
}

// defaultHarnessName is the harness used when `omac start`/`omac serve` is
// invoked without a positional harness token. It preserves the historical
// behavior (OpenCode).
const defaultHarnessName = "opencode"

// harnessRegistry is the single source of truth for supported harnesses.
// Order is significant only for the human-readable list in error messages
// (canonical names are sorted there).
func harnessRegistry() []Harness {
	return []Harness{
		{
			Name:         "opencode",
			Aliases:      []string{"oc"},
			InnerCmd:     []string{"opencode"},
			ServerLaunch: &ServerLaunch{Subcommand: "serve"},
			BridgeDir:    filepath.Join(".opencode", "plugins"),
			SkillsBase:   "opencode",
			Session: &HarnessSession{
				ContinueArgs:   []string{"--continue"},
				ResumeByIDArgs: func(id string) []string { return []string{"--session", id} },
				ListKind:       SessionListOpenCodeCLI,
			},
		},
		{
			Name:    "claude-code",
			Aliases: []string{"claude", "cc"},
			// Claude Code's CLI executable is `claude`.
			InnerCmd: []string{"claude"},
			// Claude Code has no `opencode serve`-style daemon convention in
			// scope for this change; under `omac serve` it runs as-is. If a
			// stable server/headless mode is adopted later, declare it here.
			ServerLaunch: nil,
			BridgeDir:    ".claude",
			SkillsBase:   "claude",
			// Claude Code's config home is ~/.claude, not ~/.config/claude,
			// so its global skills live in ~/.claude/skills.
			UserConfigHome: ".claude",
			Session: &HarnessSession{
				ContinueArgs:   []string{"--continue"},
				ResumeByIDArgs: func(id string) []string { return []string{"--resume", id} },
				ListKind:       SessionListClaudeFiles,
			},
		},
	}
}

// DefaultHarness returns the harness used when no harness token is given.
func DefaultHarness() Harness {
	h, ok := LookupHarness(defaultHarnessName)
	if !ok {
		// Unreachable: the default is always registered. Fail loud in tests
		// rather than silently returning a zero value.
		panic("config: default harness " + defaultHarnessName + " not registered")
	}
	return h
}

// LookupHarness resolves a harness by canonical name or alias
// (case-insensitive). The second return is false if the name is unknown.
func LookupHarness(name string) (Harness, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return Harness{}, false
	}
	for _, h := range harnessRegistry() {
		if strings.ToLower(h.Name) == want {
			return h, true
		}
		for _, a := range h.Aliases {
			if strings.ToLower(a) == want {
				return h, true
			}
		}
	}
	return Harness{}, false
}

// IsHarnessName reports whether token names a known harness (canonical or
// alias). Used by the CLI to decide whether a leading positional token is a
// harness selector or an inner-command argument.
func IsHarnessName(token string) bool {
	_, ok := LookupHarness(token)
	return ok
}

// HarnessNames returns the canonical harness names, sorted, for help text
// and error messages.
func HarnessNames() []string {
	reg := harnessRegistry()
	names := make([]string, 0, len(reg))
	for _, h := range reg {
		names = append(names, h.Name)
	}
	sort.Strings(names)
	return names
}

// UnknownHarnessError formats a consistent error for an unrecognized harness
// token, listing the supported names.
func UnknownHarnessError(name string) error {
	return fmt.Errorf("unknown harness %q (supported: %s)", name, strings.Join(HarnessNames(), ", "))
}

// AllHarnesses returns every registered harness, in registry order. Used by
// skill discovery to enumerate harness scopes (e.g. for cross-harness
// ambiguity detection at register time).
func AllHarnesses() []Harness {
	return harnessRegistry()
}

// AllHarnessSkillsBases returns every harness's own SkillsBase (e.g.
// "opencode", "claude"). Used by skill discovery to know which bases belong
// to *some* harness, so the inactive harness's base can be excluded.
func AllHarnessSkillsBases() []string {
	reg := harnessRegistry()
	out := make([]string, 0, len(reg))
	for _, h := range reg {
		if h.SkillsBase != "" {
			out = append(out, h.SkillsBase)
		}
	}
	return out
}

// InScopeSkillsBases returns the directory bases omac scans for skills when
// this harness is active: the harness's own SkillsBase followed by the shared
// neutral base. The own base is first so it ranks above the shared base on a
// name collision. A harness's scope never includes another harness's base.
func (h Harness) InScopeSkillsBases() []string {
	if h.SkillsBase == "" || h.SkillsBase == SharedSkillsBase {
		return []string{SharedSkillsBase}
	}
	return []string{h.SkillsBase, SharedSkillsBase}
}

// SkillsBaseInScope reports whether a skills directory base (e.g. "opencode",
// "claude", "agents") is in scope when this harness is active.
func (h Harness) SkillsBaseInScope(base string) bool {
	for _, b := range h.InScopeSkillsBases() {
		if b == base {
			return true
		}
	}
	return false
}

// WorkdirSkillsDir returns this harness's workdir-relative skills directory
// (e.g. ".opencode/skills", ".claude/skills"). This is the default install
// target for the active harness so installed skills land where the harness's
// own loader reads them.
func (h Harness) WorkdirSkillsDir() string {
	base := h.SkillsBase
	if base == "" {
		base = SharedSkillsBase
	}
	return filepath.Join("."+base, "skills")
}

// GlobalBridgeDir returns the absolute, user-global directory where this
// harness loads bridge plugins from, mirroring BridgeDir but rooted at the
// harness's user config dir instead of a project. For OpenCode this is
// ~/.config/opencode/plugins (honoring $XDG_CONFIG_HOME), matching
// OpenCode's documented global plugin location. The leaf (e.g. "plugins")
// is taken from BridgeDir so the two stay in lockstep.
//
// It returns "" when the harness has no bridge directory or when no home
// directory can be resolved.
func (h Harness) GlobalBridgeDir() string {
	if h.BridgeDir == "" {
		return ""
	}
	base := h.SkillsBase
	if base == "" {
		base = SharedSkillsBase
	}
	// The bridge leaf is the final path element of BridgeDir
	// (".opencode/plugins" -> "plugins", ".claude" -> ".claude"). For a
	// single-element bridge dir that is itself the config base (Claude
	// Code's ".claude"), there is no nested plugin leaf, so global bridge
	// installation is not modeled; return "".
	leaf := filepath.Base(h.BridgeDir)
	if leaf == "."+base || leaf == base {
		return ""
	}
	root := userConfigRoot()
	if root == "" {
		return ""
	}
	return filepath.Join(root, base, leaf)
}

// GlobalSkillsDir returns the absolute user-global skills directory that THIS
// harness's own loader reads — the place a guidance-only skill must be written
// to be surfaced by the harness (omac does not register or activate such
// skills). OpenCode follows XDG (~/.config/opencode/skills, $XDG_CONFIG_HOME
// honored); Claude Code uses ~/.claude/skills (see UserConfigHome).
//
// It returns "" when no home/config directory can be resolved.
func (h Harness) GlobalSkillsDir() string {
	base := h.SkillsBase
	if base == "" {
		base = SharedSkillsBase
	}
	if h.UserConfigHome != "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		return filepath.Join(home, h.UserConfigHome, "skills")
	}
	root := userConfigRoot()
	if root == "" {
		return ""
	}
	return filepath.Join(root, base, "skills")
}

// userConfigRoot resolves the base user config directory, honoring
// $XDG_CONFIG_HOME and falling back to $HOME/.config. It returns "" when
// neither is available.
func userConfigRoot() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return xdg
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config")
}

// ApplyServerLaunch ensures the inner command launches this harness's server
// when one is defined. It mirrors the previous opencode-specific
// ensureServeSubcommand logic, but driven by the descriptor: if a Subcommand
// is declared and neither the inner tail nor the trailing args already begin
// with a subcommand, it is inserted right after the executable. Harnesses
// without a ServerLaunch return inner unchanged.
func (h Harness) ApplyServerLaunch(inner, trailing []string) []string {
	if h.ServerLaunch == nil || h.ServerLaunch.Subcommand == "" {
		return inner
	}
	if len(inner) == 0 {
		return inner
	}
	if hasLeadingSubcommand(inner[1:]) || hasLeadingSubcommand(trailing) {
		return inner
	}
	out := make([]string, 0, len(inner)+1)
	out = append(out, inner[0], h.ServerLaunch.Subcommand)
	out = append(out, inner[1:]...)
	return out
}

// ResolveInnerCmd computes the inner command argv for a launch, applying the
// precedence the CLI needs:
//
//  1. An explicit --inner override always supplies the executable. When a
//     profile inner_cmd exists, the override replaces only the executable and
//     keeps the profile's default arguments; otherwise it stands alone.
//  2. Otherwise the profile's inner_cmd is used IF the profile pins one. The
//     default sandbox profiles ship with an EMPTY inner_cmd precisely so they
//     do NOT pin a harness — only a user who set inner_cmd in their own config
//     (or the debug `bash` profile) reaches this branch.
//  3. Otherwise the selected harness's default InnerCmd is used. This is the
//     common path: `omac start claude` → ["claude"], `omac start` → ["opencode"].
//
// profileInner is the resolved profile's InnerCmd (may be nil when running
// --no-sandbox / --no-inner with no profile). override is the value of
// --inner (empty if unset).
func (h Harness) ResolveInnerCmd(profileInner []string, override string) []string {
	inner := append([]string(nil), profileInner...)
	if override != "" {
		if len(inner) == 0 {
			inner = []string{override}
		} else {
			inner = append([]string{override}, inner[1:]...)
		}
		return inner
	}
	if len(inner) == 0 {
		inner = append([]string(nil), h.InnerCmd...)
	}
	return inner
}

// hasLeadingSubcommand reports whether args begins with a non-flag positional
// (a subcommand). A leading flag (or empty input) means "no subcommand".
func hasLeadingSubcommand(args []string) bool {
	for _, a := range args {
		if a == "" {
			continue
		}
		if a[0] == '-' {
			return false
		}
		return true
	}
	return false
}
