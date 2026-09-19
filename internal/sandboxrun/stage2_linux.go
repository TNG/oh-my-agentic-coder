//go:build linux

package sandboxrun

import (
	"fmt"
	"strconv"
)

// RunStage2 is the entry point for the hidden `omac sandbox stage2`
// subcommand, executed by bwrap inside the mount/pid namespaces. It
// parses --connect-tcp/--bind-tcp/--rw-grant/--enforce flags, applies
// the Landlock FS ruleset (read-only grant enforcement) and the
// Landlock net ruleset when --enforce is present, and execs the inner
// command. Returns only on error.
func RunStage2(args []string) error {
	var connect, bind []int
	var rwGrants []string
	enforce := false
	var inner []string
	i := 0
	for ; i < len(args); i++ {
		switch args[i] {
		case "--connect-tcp", "--bind-tcp":
			if i+1 >= len(args) {
				return fmt.Errorf("stage2: %s requires a value", args[i])
			}
			port, err := strconv.Atoi(args[i+1])
			if err != nil || port < 1 || port > 65535 {
				return fmt.Errorf("stage2: invalid port %q", args[i+1])
			}
			if args[i] == "--connect-tcp" {
				connect = append(connect, port)
			} else {
				bind = append(bind, port)
			}
			i++
		case "--rw-grant":
			if i+1 >= len(args) {
				return fmt.Errorf("stage2: %s requires a value", args[i])
			}
			rwGrants = append(rwGrants, args[i+1])
			i++
		case "--enforce":
			enforce = true
		case "--":
			inner = args[i+1:]
			i = len(args)
		default:
			return fmt.Errorf("stage2: unknown flag %q", args[i])
		}
	}
	if len(inner) == 0 {
		return fmt.Errorf("stage2: no inner command after --")
	}
	if enforce {
		if err := ApplyLandlockFS(rwGrants); err != nil {
			return fmt.Errorf("stage2: %w", err)
		}
		if err := ApplyLandlockNet(connect, bind); err != nil {
			return fmt.Errorf("stage2: %w", err)
		}
		// ApplyLandlockNet already set no_new_privs; block datagrams now
		// so UDP/ICMP cannot bypass the Landlock TCP filter.
		if err := applyDatagramSeccomp(); err != nil {
			return fmt.Errorf("stage2: %w", err)
		}
	}
	return ExecInner(inner)
}
