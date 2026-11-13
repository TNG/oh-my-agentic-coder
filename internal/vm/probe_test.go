package vm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

func serverPort(t *testing.T, h http.HandlerFunc) int {
	t.Helper()
	sectest.RequireLoopbackListener(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func TestDefaultDockerProbeAcceptsDocker(t *testing.T) {
	var path string
	port := serverPort(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ApiVersion":"1.47","Version":"29.9.2"}`))
	})
	v, err := DefaultDockerProbe(context.Background(), port)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if v != (DockerVersion{APIVersion: "1.47", Version: "29.9.2"}) {
		t.Errorf("version = %+v", v)
	}
	if path != "/version" {
		t.Errorf("path = %q, want /version", path)
	}
}

func TestDefaultDockerProbeRejectsNonDocker(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"html 200": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>hello</html>"))
		},
		"404": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"json without ApiVersion": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"Version":"1.0"}`))
		},
		"docker json with 500": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ApiVersion":"1.47","Version":"29.9.2"}`))
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			port := serverPort(t, h)
			_, err := DefaultDockerProbe(context.Background(), port)
			if !errors.Is(err, ErrNotDocker) {
				t.Fatalf("err = %v, want ErrNotDocker", err)
			}
		})
	}
}

func TestDefaultDockerProbeClosedPort(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	_, err = DefaultDockerProbe(context.Background(), port)
	if err == nil {
		t.Fatal("probe of closed port returned nil")
	}
	if errors.Is(err, ErrNotDocker) {
		t.Fatalf("closed port reported as ErrNotDocker: %v", err)
	}
}

func TestWaitForDockerRetriesConnectionErrors(t *testing.T) {
	calls := 0
	probe := func(context.Context, int) (DockerVersion, error) {
		calls++
		if calls <= 2 {
			return DockerVersion{}, errors.New("connection refused")
		}
		return DockerVersion{APIVersion: "1.47"}, nil
	}
	v, err := WaitForDocker(context.Background(), 1, time.Millisecond, probe)
	if err != nil {
		t.Fatal(err)
	}
	if v.APIVersion != "1.47" || calls != 3 {
		t.Errorf("v = %+v, calls = %d", v, calls)
	}
}

func TestWaitForDockerStopsOnNotDocker(t *testing.T) {
	calls := 0
	probe := func(context.Context, int) (DockerVersion, error) {
		calls++
		return DockerVersion{}, ErrNotDocker
	}
	_, err := WaitForDocker(context.Background(), 1, time.Millisecond, probe)
	if !errors.Is(err, ErrNotDocker) {
		t.Fatalf("err = %v, want ErrNotDocker", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestWaitForDockerHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	probe := func(context.Context, int) (DockerVersion, error) {
		return DockerVersion{}, errors.New("connection refused")
	}
	_, err := WaitForDocker(ctx, 1, time.Millisecond, probe)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}
