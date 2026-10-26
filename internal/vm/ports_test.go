package vm

import (
	"net"
	"testing"
)

func TestPickPortsDefaults(t *testing.T) {
	ssh, docker, err := PickPorts(func(int) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if ssh != DefaultSSHPort || docker != DefaultDockerPort {
		t.Errorf("got %d/%d, want %d/%d", ssh, docker, DefaultSSHPort, DefaultDockerPort)
	}
}

func TestPickPortsSkipsBusySSHDefault(t *testing.T) {
	ssh, docker, err := PickPorts(func(p int) bool { return p != 42022 })
	if err != nil {
		t.Fatal(err)
	}
	if ssh != 42023 || docker != DefaultDockerPort {
		t.Errorf("got %d/%d, want 42023/%d", ssh, docker, DefaultDockerPort)
	}
}

func TestPickPortsNeverEqual(t *testing.T) {
	// Only ports at or above 42376 are free: the SSH scan lands in the docker
	// range, and the docker pick must not reuse it.
	ssh, docker, err := PickPorts(func(p int) bool { return p >= 42376 })
	if err != nil {
		t.Fatal(err)
	}
	if ssh == docker {
		t.Fatalf("ssh and docker port both %d", ssh)
	}
}

func TestPickPortsExhausted(t *testing.T) {
	if _, _, err := PickPorts(func(int) bool { return false }); err == nil {
		t.Fatal("PickPorts with no free port returned nil error")
	}
	// Only one port free in total: two distinct ports cannot be found.
	if _, _, err := PickPorts(func(p int) bool { return p == 42500 }); err == nil {
		t.Fatal("PickPorts with a single free port returned nil error")
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
