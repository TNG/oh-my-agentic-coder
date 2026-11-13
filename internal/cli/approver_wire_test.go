package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The test stub answers every dialog silently, so a start with it set must
// say so.
func TestSidecarApproverWarnsAboutTheStub(t *testing.T) {
	t.Setenv("OMAC_APPROVAL_STUB", "Allow once")
	var buf bytes.Buffer
	newSidecarApprover(&buf, "omac start", nil)
	if !strings.Contains(buf.String(), "OMAC_APPROVAL_STUB is set") {
		t.Errorf("no warning: %q", buf.String())
	}
	t.Setenv("OMAC_APPROVAL_STUB", "")
	buf.Reset()
	newSidecarApprover(&buf, "omac start", nil)
	if strings.Contains(buf.String(), "OMAC_APPROVAL_STUB") {
		t.Errorf("warned without the stub: %q", buf.String())
	}
}
