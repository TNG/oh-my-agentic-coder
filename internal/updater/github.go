package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ReleaseSource fetches metadata about the latest published release.
type ReleaseSource interface {
	LatestRelease(ctx context.Context) (Release, error)
}

type githubReleaseSource struct {
	client      *http.Client
	baseURL     string // overridable for tests (httptest.Server)
	owner, repo string
}

// NewGitHubReleaseSource returns a ReleaseSource backed by the real GitHub
// API, targeting TNG/oh-my-agentic-coder's /releases/latest endpoint (which
// already excludes prereleases).
func NewGitHubReleaseSource() ReleaseSource {
	return &githubReleaseSource{
		client:  &http.Client{Timeout: 15 * time.Second},
		baseURL: "https://api.github.com",
		owner:   "TNG",
		repo:    "oh-my-agentic-coder",
	}
}

type githubReleaseJSON struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (s *githubReleaseSource) LatestRelease(ctx context.Context) (Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", s.baseURL, s.owner, s.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub API returned %s", resp.Status)
	}
	var body githubReleaseJSON
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("decode release: %w", err)
	}
	rel := Release{TagName: body.TagName}
	for _, a := range body.Assets {
		rel.Assets = append(rel.Assets, Asset{Name: a.Name, BrowserDownloadURL: a.BrowserDownloadURL})
	}
	return rel, nil
}
