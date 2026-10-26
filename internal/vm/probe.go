package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DockerVersion is the part of Docker's GET /version answer omac checks.
type DockerVersion struct {
	APIVersion string `json:"ApiVersion"`
	Version    string `json:"Version"`
}

// ErrNotDocker means something listens on the port but it is not Docker,
// for example another local service that took the port.
var ErrNotDocker = errors.New("endpoint answered but is not a Docker API")

// DockerProbe asks the forwarded Docker port on 127.0.0.1 for its version.
type DockerProbe func(ctx context.Context, port int) (DockerVersion, error)

// DefaultDockerProbe calls GET http://127.0.0.1:<port>/version. Only a 200
// answer with a JSON body carrying a non-empty ApiVersion counts as Docker;
// any other answer yields ErrNotDocker. Connection failures are returned
// as they are, so callers can keep waiting.
func DefaultDockerProbe(ctx context.Context, port int) (DockerVersion, error) {
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return DockerVersion{}, err
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return DockerVersion{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return DockerVersion{}, err
	}
	var v DockerVersion
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &v) != nil || v.APIVersion == "" {
		return DockerVersion{}, fmt.Errorf("port %d: %w", port, ErrNotDocker)
	}
	return v, nil
}

// WaitForDocker polls probe every interval until Docker answers or ctx ends.
// ErrNotDocker stops the wait at once: a wrong service is answering, and
// waiting longer will not change that.
func WaitForDocker(ctx context.Context, port int, interval time.Duration, probe DockerProbe) (DockerVersion, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		v, err := probe(ctx, port)
		if err == nil {
			return v, nil
		}
		if errors.Is(err, ErrNotDocker) {
			return DockerVersion{}, err
		}
		select {
		case <-ctx.Done():
			return DockerVersion{}, fmt.Errorf("omac vm: docker on port %d not ready (last error: %v): %w", port, err, ctx.Err())
		case <-ticker.C:
		}
	}
}
