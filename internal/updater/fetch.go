package updater

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Fetcher downloads release assets over HTTP.
type Fetcher interface {
	// FetchAll downloads url fully into memory. Used for small files
	// (checksums.txt).
	FetchAll(ctx context.Context, url string) ([]byte, error)
	// FetchToFile streams url's body into a new temp file under dir (using
	// pattern as os.CreateTemp's pattern) and returns its path. Used for
	// package/tarball assets, which may be several MB.
	FetchToFile(ctx context.Context, url, dir, pattern string) (path string, err error)
}

type httpFetcher struct {
	client *http.Client
}

// NewHTTPFetcher returns a Fetcher backed by the real network.
func NewHTTPFetcher() Fetcher {
	return httpFetcher{client: &http.Client{Timeout: 60 * time.Second}}
}

func (f httpFetcher) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

func (f httpFetcher) FetchAll(ctx context.Context, url string) ([]byte, error) {
	body, err := f.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

func (f httpFetcher) FetchToFile(ctx context.Context, url, dir, pattern string) (string, error) {
	body, err := f.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer body.Close()

	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := io.Copy(tmp, body); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}
