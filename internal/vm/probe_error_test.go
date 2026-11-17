package vm

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitForDockerKeepsProbeError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probeErr := errors.New("connection refused")
	probe := func(context.Context, int) (DockerVersion, error) {
		return DockerVersion{}, probeErr
	}
	_, err := WaitForDocker(ctx, 1, time.Millisecond, probe)
	if !errors.Is(err, probeErr) {
		t.Errorf("err = %v, want the probe error in the chain", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled in the chain", err)
	}
}
