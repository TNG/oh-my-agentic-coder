package vm

import (
	"errors"
	"net"
	"strconv"
)

// DefaultSSHPort is the host port on 127.0.0.1 forwarded to the VM's ssh.
const DefaultSSHPort = 42022

// lastScanPort is the upper end of the fallback scan.
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

// PickSSHPort returns DefaultSSHPort when free; otherwise the first free
// port in 42023..42999.
func PickSSHPort(free func(int) bool) (int, error) {
	for p := DefaultSSHPort; p <= lastScanPort; p++ {
		if free(p) {
			return p, nil
		}
	}
	return 0, errors.New("omac vm: no free loopback port for ssh in 42022..42999")
}
