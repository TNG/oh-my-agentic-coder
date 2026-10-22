package vm

import (
	"fmt"
	"path/filepath"
	"strconv"
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
