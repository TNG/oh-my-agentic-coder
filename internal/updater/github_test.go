package updater

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGithubReleaseSource_LatestRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/TNG/oh-my-agentic-coder/releases/latest" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v1.2.3",
			"assets": []map[string]any{
				{"name": "oh-my-agentic-coder_1.2.3_linux_x86_64.deb", "browser_download_url": "https://example.invalid/a.deb"},
				{"name": "checksums.txt", "browser_download_url": "https://example.invalid/checksums.txt"},
			},
		})
	}))
	defer srv.Close()

	src := &githubReleaseSource{client: srv.Client(), baseURL: srv.URL, owner: "TNG", repo: "oh-my-agentic-coder"}
	rel, err := src.LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.TagName != "v1.2.3" {
		t.Fatalf("TagName = %q", rel.TagName)
	}
	if len(rel.Assets) != 2 || rel.Assets[0].Name != "oh-my-agentic-coder_1.2.3_linux_x86_64.deb" {
		t.Fatalf("Assets = %+v", rel.Assets)
	}
}

func TestGithubReleaseSource_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	src := &githubReleaseSource{client: srv.Client(), baseURL: srv.URL, owner: "TNG", repo: "oh-my-agentic-coder"}
	if _, err := src.LatestRelease(context.Background()); err == nil {
		t.Fatalf("expected error for non-200 response")
	}
}
