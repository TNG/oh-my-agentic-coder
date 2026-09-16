package audit

import (
	"strings"
	"testing"
)

// recordSizeCap is the largest marshalled record this test tolerates.
// Chosen well above any legitimate event, well below the >4096-byte
// threshold that forces the fileSink's body+newline write onto two
// separate syscalls (see TestSecurityAuditRecordIsWrittenAtomically).
const recordSizeCap = 4096

// TestSecurityAgentCannotChooseRecordSize asserts that fields an agent
// controls (a facade request path, a sidecar's exec argv) cannot make a
// single audit record arbitrarily large.
//
// FacadeRequest and InnerExec copy their string/slice inputs into an Event
// with no length cap. A record past the fileSink's ~4096-byte buffer size
// forces the body and its trailing newline onto two separate write(2)
// calls, which is what lets a concurrent writer interleave a record
// between them — the mechanism behind 3748 of 6000 records vanishing under
// two-sink contention. An agent that controls the request path or exec
// argv chooses whether that window opens.
func TestSecurityAgentCannotChooseRecordSize(t *testing.T) {
	// Control: an ordinary, short path marshals well under the cap, so the
	// cap itself doesn't reject normal events.
	normal, err := marshalLine(FacadeRequest("GET", "m", "ns", "/status", 200, 0, 1))
	if err != nil {
		t.Fatalf("marshalLine: %v", err)
	}
	if len(normal) > recordSizeCap {
		t.Fatalf("control: an ordinary short-path event already exceeds the cap (%d bytes): the fixture is broken, not the security property", len(normal))
	}

	hostilePath := "/" + strings.Repeat("a", 1<<16)
	big, err := marshalLine(FacadeRequest("GET", "m", "ns", hostilePath, 200, 0, 1))
	if err != nil {
		t.Fatalf("marshalLine: %v", err)
	}
	if len(big) > recordSizeCap {
		t.Errorf("a facade.request event with an agent-controlled path of %d bytes marshalled to %d bytes, well past the %d-byte cap: "+
			"nothing truncates or caps agent-controlled fields before they become one audit record", len(hostilePath), len(big), recordSizeCap)
	}
}
