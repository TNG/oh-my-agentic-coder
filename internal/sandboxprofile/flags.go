package sandboxprofile

import (
	"fmt"
	"strconv"
	"strings"
)

// Flags captures the `omac sandbox run` command-line overrides. All
// list-valued flags are repeatable and merge additively into the
// loaded profile; --block-net and --workdir-access replace.
type Flags struct {
	ProfileRef    string
	Allow         []string // --allow <path>        read+write dir/file
	Read          []string // --read <path>         read-only
	Write         []string // --write <path>        write-only
	Deny          []string // --deny <path|glob>    mask within granted trees
	AllowFile     []string // --allow-file <path>   read+write single file
	AllowUnixDir  []string // --allow-unix-dir <dir> AF_UNIX connect to any socket under dir
	OpenPort      []int    // --open-port <port>
	ListenPort    []int    // --listen-port <port>
	AllowTCP      []int    // --allow-tcp-connect <port>
	AllowDomain   []string // --allow-domain <domain>
	DenyDomain    []string // --deny-domain <domain>
	BlockNet      bool     // --block-net
	Learn         bool     // --learn: unrestricted fs + folder recording
	WorkdirAccess string   // --workdir-access <level>
	InnerArgv     []string // everything after --
}

// ParseFlags parses the argument vector for `omac sandbox run` (the
// portion after the subcommand words). The inner command follows "--".
func ParseFlags(args []string) (*Flags, error) {
	f := &Flags{}
	i := 0
	next := func(flag string) (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("%s requires a value", flag)
		}
		i++
		return args[i], nil
	}
	nextPort := func(flag string) (int, error) {
		v, err := next(flag)
		if err != nil {
			return 0, err
		}
		port, err := strconv.Atoi(v)
		if err != nil || port < 1 || port > 65535 {
			return 0, fmt.Errorf("%s: invalid port %q", flag, v)
		}
		return port, nil
	}
	for ; i < len(args); i++ {
		a := args[i]
		// Accept --flag=value for every flag.
		var inline string
		hasInline := false
		if idx := strings.Index(a, "="); idx > 0 && strings.HasPrefix(a, "--") {
			inline = a[idx+1:]
			a = a[:idx]
			hasInline = true
		}
		val := func(flag string) (string, error) {
			if hasInline {
				return inline, nil
			}
			return next(flag)
		}
		portVal := func(flag string) (int, error) {
			if hasInline {
				port, err := strconv.Atoi(inline)
				if err != nil || port < 1 || port > 65535 {
					return 0, fmt.Errorf("%s: invalid port %q", flag, inline)
				}
				return port, nil
			}
			return nextPort(flag)
		}
		switch a {
		case "--":
			if hasInline {
				return nil, fmt.Errorf("malformed argument %q", args[i])
			}
			f.InnerArgv = args[i+1:]
			if len(f.InnerArgv) == 0 {
				return nil, fmt.Errorf("no command after --")
			}
			return f, nil
		case "--profile":
			v, err := val(a)
			if err != nil {
				return nil, err
			}
			f.ProfileRef = v
		case "--allow":
			v, err := val(a)
			if err != nil {
				return nil, err
			}
			f.Allow = append(f.Allow, v)
		case "--read":
			v, err := val(a)
			if err != nil {
				return nil, err
			}
			f.Read = append(f.Read, v)
		case "--write":
			v, err := val(a)
			if err != nil {
				return nil, err
			}
			f.Write = append(f.Write, v)
		case "--deny":
			v, err := val(a)
			if err != nil {
				return nil, err
			}
			f.Deny = append(f.Deny, v)
		case "--allow-file":
			v, err := val(a)
			if err != nil {
				return nil, err
			}
			f.AllowFile = append(f.AllowFile, v)
		case "--allow-unix-dir":
			v, err := val(a)
			if err != nil {
				return nil, err
			}
			f.AllowUnixDir = append(f.AllowUnixDir, v)
		case "--open-port":
			p, err := portVal(a)
			if err != nil {
				return nil, err
			}
			f.OpenPort = append(f.OpenPort, p)
		case "--listen-port":
			p, err := portVal(a)
			if err != nil {
				return nil, err
			}
			f.ListenPort = append(f.ListenPort, p)
		case "--allow-tcp-connect":
			p, err := portVal(a)
			if err != nil {
				return nil, err
			}
			f.AllowTCP = append(f.AllowTCP, p)
		case "--allow-domain":
			v, err := val(a)
			if err != nil {
				return nil, err
			}
			f.AllowDomain = append(f.AllowDomain, v)
		case "--deny-domain":
			v, err := val(a)
			if err != nil {
				return nil, err
			}
			f.DenyDomain = append(f.DenyDomain, v)
		case "--block-net":
			if hasInline {
				return nil, fmt.Errorf("--block-net takes no value")
			}
			f.BlockNet = true
		case "--learn":
			if hasInline {
				return nil, fmt.Errorf("--learn takes no value")
			}
			f.Learn = true
		case "--workdir-access":
			v, err := val(a)
			if err != nil {
				return nil, err
			}
			switch v {
			case AccessNone, AccessRead, AccessWrite, AccessReadWrite:
			default:
				return nil, fmt.Errorf("--workdir-access: invalid level %q (want none|read|write|readwrite)", v)
			}
			f.WorkdirAccess = v
		default:
			return nil, fmt.Errorf("unknown flag %q (inner command goes after --)", args[i])
		}
	}
	return nil, fmt.Errorf("missing -- separator and inner command")
}

// Merge applies the flag overrides onto a copy of the profile and
// returns it together with any warnings to print. List-valued flags
// merge additively; --block-net forces network.mode=blocked (warning
// when it overrides other network settings); --workdir-access replaces.
func Merge(p *Profile, f *Flags) (*Profile, []string) {
	out := *p // shallow copy; slices below are re-appended, never mutated in place
	var warnings []string

	out.Filesystem.Allow = appendCopy(p.Filesystem.Allow, append(f.Allow, f.AllowFile...)...)
	out.Filesystem.Read = appendCopy(p.Filesystem.Read, f.Read...)
	out.Filesystem.Write = appendCopy(p.Filesystem.Write, f.Write...)
	out.Filesystem.AllowUnixDir = appendCopy(p.Filesystem.AllowUnixDir, f.AllowUnixDir...)
	out.Filesystem.Deny = appendCopy(p.Filesystem.Deny, f.Deny...)

	out.Network.OpenPort = appendIntCopy(p.Network.OpenPort, f.OpenPort...)
	out.Network.ListenPort = appendIntCopy(p.Network.ListenPort, f.ListenPort...)
	out.Network.AllowTCPConnect = appendIntCopy(p.Network.AllowTCPConnect, f.AllowTCP...)
	out.Network.AllowDomain = appendCopy(p.Network.AllowDomain, f.AllowDomain...)
	out.Network.DenyDomain = appendCopy(p.Network.DenyDomain, f.DenyDomain...)

	if f.WorkdirAccess != "" {
		out.Workdir.Access = f.WorkdirAccess
	}
	if f.BlockNet {
		if out.Network.EffectiveMode() != ModeBlocked &&
			(len(out.Network.AllowDomain) > 0 || len(out.Network.DenyDomain) > 0 || out.PromptConfigured()) {
			warnings = append(warnings,
				"--block-net overrides the profile's network settings; network remains fully blocked")
		}
		out.Network.Mode = ModeBlocked
	}
	return &out, warnings
}

// PromptConfigured reports whether the profile has an (enabled) prompt block.
func (p *Profile) PromptConfigured() bool {
	return p.Network.PromptEnabled()
}

func appendCopy(base []string, extra ...string) []string {
	if len(extra) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}

func appendIntCopy(base []int, extra ...int) []int {
	if len(extra) == 0 {
		return base
	}
	out := make([]int, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}
