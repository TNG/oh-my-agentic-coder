package vm

import (
	"net"
	"testing"
)

func TestPickSSHPortDefault(t *testing.T) {
	ssh, err := PickSSHPort(func(int) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if ssh != DefaultSSHPort {
		t.Errorf("got %d, want %d", ssh, DefaultSSHPort)
	}
}

func TestPickSSHPortSkipsBusyDefault(t *testing.T) {
	ssh, err := PickSSHPort(func(p int) bool { return p != 42022 })
	if err != nil {
		t.Fatal(err)
	}
	if ssh != 42023 {
		t.Errorf("got %d, want 42023", ssh)
	}
}

func TestPickSSHPortExhausted(t *testing.T) {
	if _, err := PickSSHPort(func(int) bool { return false }); err == nil {
		t.Fatal("PickSSHPort with no free port returned nil error")
	}
	// A free port above the scan range does not count.
	if _, err := PickSSHPort(func(p int) bool { return p == 43000 }); err == nil {
		t.Fatal("PickSSHPort picked a port outside 42022..42999")
	}
}

func TestPortFreeOnHeldListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a loopback listener: %v", err)
	}
	defer ln.Close()
	if PortFree(ln.Addr().(*net.TCPAddr).Port) {
		t.Error("PortFree reported a held port as free")
	}
}
