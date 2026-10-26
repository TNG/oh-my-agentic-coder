package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Environment variables omac exports into a session attached to the VM.
const (
	EnvVMHost    = "OMAC_VM_HOST"
	EnvVMSSHPort = "OMAC_VM_SSH_PORT"
	EnvVMSSHKey  = "OMAC_VM_SSH_KEY"
	EnvVMUser    = "OMAC_VM_USER"
)

// ConnInfo is how to reach the agent user in the VM. FromSandbox is true
// when the values came from the session environment, not the state dir.
type ConnInfo struct {
	Target      SSHTarget
	FromSandbox bool
}

var vmUserRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ConnFromEnv reads the OMAC_VM_* variables. ok is false when
// OMAC_VM_SSH_PORT is unset; once it is set, the key and user must be set
// too. The known_hosts file sits next to the copied key, because the
// sandbox cannot read the protected state directory.
func ConnFromEnv(getenv func(string) string) (ConnInfo, bool, error) {
	portStr := getenv(EnvVMSSHPort)
	if portStr == "" {
		return ConnInfo{}, false, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return ConnInfo{}, true, fmt.Errorf("omac vm: %s=%q is not a port number", EnvVMSSHPort, portStr)
	}
	key, user := getenv(EnvVMSSHKey), getenv(EnvVMUser)
	if key == "" || user == "" {
		return ConnInfo{}, true, fmt.Errorf("omac vm: %s is set but %s or %s is missing", EnvVMSSHPort, EnvVMSSHKey, EnvVMUser)
	}
	// The user goes before "--" on the ssh command line, and a space would
	// split the key's known_hosts path in UserKnownHostsFile.
	if !vmUserRE.MatchString(user) {
		return ConnInfo{}, true, fmt.Errorf("omac vm: %s=%q is not a valid user name", EnvVMUser, user)
	}
	if !filepath.IsAbs(key) || strings.IndexFunc(key, unicode.IsSpace) >= 0 {
		return ConnInfo{}, true, fmt.Errorf("omac vm: %s=%q must be an absolute path without whitespace", EnvVMSSHKey, key)
	}
	return ConnInfo{
		Target: SSHTarget{
			User:       user,
			Port:       port,
			KeyPath:    key,
			KnownHosts: filepath.Join(filepath.Dir(key), "known_hosts"),
		},
		FromSandbox: true,
	}, true, nil
}

// ConnFromState builds the target from the host-side state directory.
func ConnFromState(l Layout, hostUser string) (ConnInfo, error) {
	st, err := LoadState(l.StatePath)
	if errors.Is(err, os.ErrPermission) {
		// The sandbox hides the state dir; only --vm exports a connection.
		return ConnInfo{}, fmt.Errorf("%w (inside an omac session, start it with --vm so omac vm exec can reach the VM)", err)
	}
	if err != nil {
		return ConnInfo{}, err
	}
	if st == nil {
		return ConnInfo{}, ErrNotCreated
	}
	return ConnInfo{Target: SSHTarget{
		User:       AgentUserName(hostUser),
		Port:       st.SSHPort,
		KeyPath:    l.AgentKey,
		KnownHosts: l.KnownHosts,
	}}, nil
}

// ResolveConn prefers the session environment and falls back to the state
// directory only when no OMAC_VM_* connection is exported.
func ResolveConn(getenv func(string) string, l Layout, hostUser string) (ConnInfo, error) {
	if ci, ok, err := ConnFromEnv(getenv); ok {
		return ci, err
	}
	return ConnFromState(l, hostUser)
}
