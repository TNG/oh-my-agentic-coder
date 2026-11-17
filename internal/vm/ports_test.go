package vm

import (
	"net"
	"strings"
	"testing"
)

func TestPortBlockRange(t *testing.T) {
	for block, want := range map[int][2]int{0: {30000, 30099}, 1: {30100, 30199}, 9: {30900, 30999}} {
		first, last := PortBlockRange(block)
		if first != want[0] || last != want[1] {
			t.Errorf("block %d = %d..%d, want %d..%d", block, first, last, want[0], want[1])
		}
	}
}

func TestPickPortBlock(t *testing.T) {
	cases := []struct {
		name string
		used []int
		want int
	}{
		{"none used", nil, 0},
		{"first used", []int{0}, 1},
		{"gap is filled", []int{0, 2}, 1},
		{"last free", []int{0, 1, 2, 3, 4, 5, 6, 7, 8}, 9},
	}
	for _, c := range cases {
		got, err := PickPortBlock(c.used, func(int) bool { return true })
		if err != nil || got != c.want {
			t.Errorf("%s: got %d, %v, want %d", c.name, got, err, c.want)
		}
	}
}

func TestPickPortBlockExhausted(t *testing.T) {
	_, err := PickPortBlock([]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, func(int) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "10") {
		t.Fatalf("err = %v, want an error naming the limit of 10 VMs", err)
	}
}

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
