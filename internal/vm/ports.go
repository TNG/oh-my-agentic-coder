package vm

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
)

// DefaultSSHPort is the host port on 127.0.0.1 forwarded to the VM's ssh.
const DefaultSSHPort = 42022

// lastScanPort is the upper end of the fallback scan.
const lastScanPort = 42999

// Published ports come in blocks of PortBlockSize; MaxVMs blocks fit.
const (
	publishedBase = 30000
	PortBlockSize = 100
	MaxVMs        = 10
)

// PortBlockRange is the host and guest port range that Lima forwards for
// the VM using block: the only guest ports reachable from the host
// loopback. A container published there (`docker run -p 30000:5432 …`) is
// reachable from host tools; every other guest listener stays inside the VM.
func PortBlockRange(block int) (first, last int) {
	first = publishedBase + block*PortBlockSize
	return first, first + PortBlockSize - 1
}

// PickPortBlock returns the lowest block not in used.
func PickPortBlock(used []int) (int, error) {
	for b := 0; b < MaxVMs; b++ {
		if !slices.Contains(used, b) {
			return b, nil
		}
	}
	return 0, fmt.Errorf("omac vm: all %d port blocks are taken; at most %d VMs can exist, to free one, delete a VM you no longer need: run LIMA_HOME=~/.local/state/omac/vms/<name>/lima limactl delete -f omac, then remove the directory ~/.local/state/omac/vms/<name>", MaxVMs, MaxVMs)
}

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
