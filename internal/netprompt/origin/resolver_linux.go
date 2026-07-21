//go:build linux

package origin

import (
	"bufio"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func platformResolver() Resolver { return procResolver{} }

// procResolver attributes a loopback connection via /proc. It finds the child's
// socket in /proc/net/tcp by matching the full 4-tuple (local == src AND
// rem == proxy) — matching the ephemeral local port alone is ambiguous, since
// the kernel may reuse it across connections to different remotes — then maps
// the socket inode to a PID by scanning /proc/<pid>/fd, and reads the PID's
// comm. Reads comm only; never /proc/<pid>/cmdline (argv can carry secrets).
type procResolver struct{}

func (procResolver) Resolve(src, proxy netip.AddrPort) (Origin, bool) {
	inode, ok := inodeForTuple(src, proxy)
	if !ok {
		return Origin{}, false
	}
	pid, ok := pidForInode(inode)
	if !ok {
		return Origin{}, false
	}
	name := commForPID(pid)
	if name == "" {
		return Origin{}, false
	}
	return Origin{PID: pid, Name: name}, true
}

// inodeForTuple scans /proc/net/tcp for the row whose local and remote
// endpoints equal src and proxy respectively, and returns its socket inode.
func inodeForTuple(src, proxy netip.AddrPort) (string, bool) {
	local, ok := hexEndpoint(src)
	if !ok {
		return "", false
	}
	rem, ok := hexEndpoint(proxy)
	if !ok {
		return "", false
	}
	f, err := os.Open("/proc/net/tcp")
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// 0:sl 1:local 2:rem 3:st ... 9:inode
		if len(fields) < 10 {
			continue
		}
		if strings.EqualFold(fields[1], local) && strings.EqualFold(fields[2], rem) {
			return fields[9], true
		}
	}
	return "", false
}

// hexEndpoint renders an IPv4 AddrPort in /proc/net/tcp form:
// little-endian address bytes + big-endian port, e.g. 127.0.0.1:443 ->
// "0100007F:01BB". Returns false for non-IPv4 (loopback proxy traffic is IPv4).
func hexEndpoint(ap netip.AddrPort) (string, bool) {
	a := ap.Addr().Unmap()
	if !a.Is4() {
		return "", false
	}
	b := a.As4()
	return fmt.Sprintf("%02X%02X%02X%02X:%04X", b[3], b[2], b[1], b[0], ap.Port()), true
}

// pidForInode finds the PID owning the socket with the given inode by scanning
// /proc/<pid>/fd for a "socket:[<inode>]" symlink.
func pidForInode(inode string) (int, bool) {
	target := "socket:[" + inode + "]"
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	for _, p := range procs {
		if !p.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", p.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // process gone or not ours
		}
		for _, fd := range fds {
			if link, err := os.Readlink(filepath.Join(fdDir, fd.Name())); err == nil && link == target {
				return pid, true
			}
		}
	}
	return 0, false
}

func commForPID(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
