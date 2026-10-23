package vm

import (
	"errors"
	"net"
	"strconv"
)

// Host ports on 127.0.0.1 forwarded to the VM's ssh and Docker daemon.
const (
	DefaultSSHPort    = 42022
	DefaultDockerPort = 42375
)

// lastScanPort is the upper end of both fallback scans.
const lastScanPort = 42999

// PortFree reports whether 127.0.0.1:port can be bound right now.
func PortFree(port int) bool {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// PickPorts returns the default ports when free; otherwise it scans
// 42023..42999 for ssh and 42376..42999 for Docker. The two never match.
func PickPorts(free func(int) bool) (sshPort, dockerPort int, err error) {
	sshPort = pickPort(DefaultSSHPort, free, 0)
	if sshPort == 0 {
		return 0, 0, errors.New("omac vm: no free loopback port for ssh in 42022..42999")
	}
	dockerPort = pickPort(DefaultDockerPort, free, sshPort)
	if dockerPort == 0 {
		return 0, 0, errors.New("omac vm: no free loopback port for docker in 42375..42999")
	}
	return sshPort, dockerPort, nil
}

// pickPort returns the first port from first..lastScanPort that is free
// and not taken, or 0.
func pickPort(first int, free func(int) bool, taken int) int {
	for p := first; p <= lastScanPort; p++ {
		if p != taken && free(p) {
			return p
		}
	}
	return 0
}
