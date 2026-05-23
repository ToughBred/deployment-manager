// Package git_provider provides a client for fetching deployment metadata
// published as GitHub Release assets.
//
// The deployment pipeline publishes a deployment-metadata.json file as an
// asset on each GitHub Release. This package retrieves and parses that
// metadata so the reconciler can detect drift.
//
// API endpoints used:
//
//	GET /repos/{owner}/{repo}/releases        — list releases
//	GET {asset.browser_download_url}          — download metadata asset
//
// Rate limiting: GitHub's unauthenticated API allows 60 requests/hour.
// Authenticated (token) allows 5000/hour. With a 60s poll interval and
// multiple environments, a GITHUB_TOKEN is strongly recommended in production.
package git_provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	apiBase            = "https://api.github.com"
	metadataAsset      = "deployment-metadata.json"
	defaultHTTPTimeout = 15 * time.Second
)

// release is a minimal representation of the GitHub Releases API response.
type release struct {
	TagName string  `json:"tag_name"`
	Assets  []asset `json:"assets"`
}

// asset is a file attached to a GitHub Release.
type asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// githubClient fetches deployment metadata from GitHub Releases.
type githubClient struct {
	owner string
	repo  string
	token string
	http  *http.Client
}

// NewGithubClient constructs a githubClient for the given repository.
// token may be empty for public repositories (rate limits apply).
func NewGithubClient(owner, repo, token string) GitProvider {
	return &githubClient{
		owner: owner,
		repo:  repo,
		token: token,
		http: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
	}
}

// FetchLatestForEnvironment retrieves the most recent release whose tag
// starts with releasePrefix and contains a deployment-metadata.json asset
// for the given environment.
//
// It walks releases in descending order (newest first) and returns the first
// match. This means if you tag releases as "dev-2026-05-22-abc1234", the
// latest dev release is always returned for the "dev-" prefix.
//
// Returns nil, nil if no matching release is found.
func (c *githubClient) FetchLatestForEnvironment(ctx context.Context, releasePrefix, environment string) (meta DeploymentMetadata, err error) {
	releases, err := c.listReleases(ctx)
	if err != nil {
		return meta, fmt.Errorf("failed to list releases: %w", err)
	}

	for _, r := range releases {
		if !strings.HasPrefix(r.TagName, releasePrefix) {
			continue
		}

		// Find the metadata asset in this release.
		metaURL := ""
		for _, a := range r.Assets {
			if a.Name == metadataAsset {
				metaURL = a.BrowserDownloadURL
				break
			}
		}
		if metaURL == "" {
			// Release exists but has no metadata asset — skip and continue.
			// This can happen for manually created releases or legacy tags.
			continue
		}

		meta, err = c.downloadMetadata(ctx, metaURL)
		if err != nil {
			return meta, fmt.Errorf("failed to download metadata from %q: %w", metaURL, err)
		}

		// Verify environment matches — defense against misconfigured CI.
		if meta.Environment != environment {
			continue
		}

		return meta, nil
	}

	return meta, ErrNoDeploymentMetaFound
}

// listReleases fetches the first page of releases (GitHub returns newest first).
// For repositories with many releases, pagination could be added, but the
// reconciler only needs the latest release per prefix so one page is sufficient.
func (c *githubClient) listReleases(ctx context.Context) ([]release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=30", apiBase, c.owner, c.repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize new request: %w", err)
	}
	c.addHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GitHub API %s: HTTP %d: %s", url, resp.StatusCode, body)
	}

	var releases []release
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("error decoding releases: %w", err)
	}
	return releases, nil
}

// downloadMetadata fetches and parses the deployment-metadata.json asset.
// GitHub asset download URLs redirect to blob storage; the HTTP client
// follows redirects automatically.
func (c *githubClient) downloadMetadata(ctx context.Context, url string) (meta DeploymentMetadata, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return meta, err
	}
	// Asset downloads use Accept: application/octet-stream to get raw content.
	req.Header.Set("Accept", "application/octet-stream")
	c.addHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return meta, fmt.Errorf("failed to GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return meta, fmt.Errorf("asset download failed with response code %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024)) // 64KB limit
	if err != nil {
		return meta, fmt.Errorf("failed to read asset body: %w", err)
	}

	if err = json.Unmarshal(body, &meta); err != nil {
		return meta, fmt.Errorf("parse metadata JSON: %w", err)
	}
	return meta, nil
}

func (c *githubClient) addHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
