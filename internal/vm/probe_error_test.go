package vm

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitDockerKeepsProbeError(t *testing.T) {
	m, _, _ := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probeErr := errors.New("connection refused")
	answerDocker(m, "", probeErr)
	err := m.waitDocker(ctx, &State{SSHPort: 42022}, time.Millisecond)
	if !errors.Is(err, probeErr) {
		t.Errorf("err = %v, want the probe error in the chain", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled in the chain", err)
	}
}
