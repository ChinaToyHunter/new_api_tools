package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/version"
)

// UpdaterService proxies one-click updates to a Watchtower sidecar
// (nicholas-fedor/watchtower HTTP API mode) that holds the only
// docker.sock. The app container itself never touches Docker.
//
// Required env (see .env.example / docker-compose.yml):
//   - WATCHTOWER_HTTP_API_TOKEN   shared bearer token
//   - WATCHTOWER_API_URL          e.g. http://watchtower:8080 (optional, default shown)
//
// The target container is deliberately fixed to newapi-tools.
type UpdaterService struct {
	cfg        *config.Config
	httpClient *http.Client
}

// NewUpdaterService creates the updater service.
func NewUpdaterService() *UpdaterService {
	return &UpdaterService{
		cfg: config.Get(),
		// Each operation derives its own bounded context: digest checks may take
		// several minutes, while async update triggers and GitHub calls should not.
		httpClient: &http.Client{},
	}
}

// UpdaterStatus reports whether one-click update is usable.
type UpdaterStatus struct {
	Configured      bool   `json:"configured"`
	ContainerName   string `json:"container_name"`
	CurrentVersion  string `json:"current_version"`
	VersionIsDev    bool   `json:"version_is_dev"`
	CurrentImageRef string `json:"current_image_ref"`
}

// watchtowerCheckResponse mirrors the /v1/check containers[] entries.
type watchtowerCheckResponse struct {
	Containers []watchtowerContainer `json:"containers"`
	Count      int                   `json:"count"`
}

type watchtowerContainer struct {
	Name            string `json:"name"`
	ImageName       string `json:"image"`
	Digest          string `json:"digest"`
	LatestDigest    string `json:"latest_digest"`
	UpdateAvailable bool   `json:"update_available"`
	Error           string `json:"error"`
}

// githubCommitResponse is the slice of the GitHub REST commit payload we need.
type githubCommitResponse struct {
	SHA string `json:"sha"`
}

// Status builds the status payload for GET /api/system/update/status.
func (s *UpdaterService) Status() UpdaterStatus {
	token := s.cfg.WatchtowerToken
	return UpdaterStatus{
		Configured:      token != "",
		ContainerName:   s.containerName(),
		CurrentVersion:  version.GitCommit,
		VersionIsDev:    version.GitCommit == "dev" || version.GitCommit == "",
		CurrentImageRef: s.imageRef(),
	}
}

// Check asks Watchtower to compare the running image digest against the
// registry (POST /v1/check?container=<name>) and reports update availability.
// It never pulls layers.
func (s *UpdaterService) Check(ctx context.Context) (map[string]any, error) {
	if s.cfg.WatchtowerToken == "" {
		return nil, ErrUpdaterNotConfigured
	}

	target := s.containerName()
	endpoint := fmt.Sprintf("%s/v1/check?container=%s", s.watchtowerBaseURL(), url.QueryEscape(target))
	body, code, err := s.watchtowerRequest(ctx, http.MethodPost, endpoint)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("watchtower /v1/check returned HTTP %d: %s", code, truncateBody(body))
	}

	var parsed watchtowerCheckResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析 Watchtower 响应失败: %w", err)
	}

	var entry *watchtowerContainer
	for i := range parsed.Containers {
		if parsed.Containers[i].Name == target {
			entry = &parsed.Containers[i]
			break
		}
	}

	result := map[string]any{
		"container":        target,
		"current_version":  version.GitCommit,
		"update_available": false,
		"watchtower_count": parsed.Count,
	}
	if entry != nil {
		result["update_available"] = entry.UpdateAvailable
		result["image"] = entry.ImageName
		if entry.Error != "" {
			// A per-container registry error means availability is unknown, not
			// "up to date". Return an operation error so the UI cannot invite an
			// update based on an inconclusive check.
			return nil, fmt.Errorf("watchtower check for %s failed: %s", target, entry.Error)
		}
		if entry.LatestDigest != "" {
			result["latest_digest"] = entry.LatestDigest
		}
	} else {
		return nil, fmt.Errorf("target container %q is outside Watchtower check scope", target)
	}
	return result, nil
}

// Run triggers a targeted update (POST /v1/update?container=<name>&timeout=...).
// With async=true Watchtower returns 202 immediately and recreates the app
// container in the background; the frontend then polls /api/health until the
// new container is back.
func (s *UpdaterService) Run(ctx context.Context, async bool) (map[string]any, error) {
	if s.cfg.WatchtowerToken == "" {
		return nil, ErrUpdaterNotConfigured
	}

	target := s.containerName()
	endpoint := fmt.Sprintf("%s/v1/update?container=%s&timeout=%s", s.watchtowerBaseURL(), url.QueryEscape(target), updateTimeoutParam())
	if async {
		endpoint += "&async=true"
	}

	body, code, err := s.watchtowerRequest(ctx, http.MethodPost, endpoint)
	if err != nil {
		return nil, err
	}

	// 200 = sync update finished; 202 = async accepted; 429 = another update running.
	switch code {
	case http.StatusOK, http.StatusAccepted:
		result := map[string]any{
			"triggered":   true,
			"async":       async,
			"container":   target,
			"status_code": code,
		}
		if len(body) > 0 && !async {
			var parsed map[string]any
			if json.Unmarshal(body, &parsed) == nil {
				if summary, ok := parsed["summary"]; ok {
					result["summary"] = summary
				}
			}
		}
		return result, nil
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("已有更新任务在执行中，请稍后重试 (HTTP 429)")
	default:
		return nil, fmt.Errorf("watchtower /v1/update returned HTTP %d: %s", code, truncateBody(body))
	}
}

// LatestRemoteCommit queries GitHub REST for the default branch head SHA.
// api.github.com is reachable from both the app container and CI networks;
// used as an independent version signal next to Watchtower's digest check.
func (s *UpdaterService) LatestRemoteCommit(ctx context.Context) (string, error) {
	endpoint := "https://api.github.com/repos/" + githubRepo + "/commits/main"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if s.cfg.GitHubToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.GitHubToken)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("查询 GitHub 最新提交失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}
	var parsed githubCommitResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("解析 GitHub 响应失败: %w", err)
	}
	if parsed.SHA == "" {
		return "", fmt.Errorf("GitHub 响应缺少 sha 字段")
	}
	return parsed.SHA, nil
}

// watchtowerRequest performs an authenticated request against the Watchtower API.
func (s *UpdaterService) watchtowerRequest(ctx context.Context, method, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.WatchtowerToken)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("无法连接 Watchtower (%s): %w", s.watchtowerBaseURL(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取 Watchtower 响应失败: %w", err)
	}
	return body, resp.StatusCode, nil
}

func (s *UpdaterService) watchtowerBaseURL() string {
	u := strings.TrimRight(s.cfg.WatchtowerAPIURL, "/")
	if u == "" {
		u = "http://watchtower:8080"
	}
	return u
}

func (s *UpdaterService) containerName() string {
	// The sidecar is intentionally scoped to the Compose-owned app container.
	// Do not let an environment override turn this authenticated proxy into a
	// generic host-container update primitive.
	return updaterContainerName
}

func (s *UpdaterService) imageRef() string {
	if s.cfg.WatchtowerImage != "" {
		return s.cfg.WatchtowerImage
	}
	return "ghcr.io/chinatoyhunter/new_api_tools:latest"
}

// updateTimeoutParam caps the sync wait at 5m (Watchtower's own default is 10m).
func updateTimeoutParam() string { return "5m" }

func truncateBody(b []byte) string {
	t := strings.TrimSpace(string(b))
	if len(t) > 200 {
		return t[:200] + "..."
	}
	return t
}

// ErrNoUpdateAvailable signals that Watchtower confirmed the target image is current.
// RunUpdate checks this immediately before scheduling a recreate so a no-op can never
// leave the browser waiting for a restart that will not happen.
var ErrNoUpdateAvailable = fmt.Errorf("当前镜像已是最新版本")

// ErrUpdaterNotConfigured signals missing WATCHTOWER_HTTP_API_TOKEN.
var ErrUpdaterNotConfigured = fmt.Errorf("一键更新未配置：请设置 WATCHTOWER_HTTP_API_TOKEN 并启用 watchtower sidecar")

// Package-level constants for the update checker.
const (
	githubRepo           = "ChinaToyHunter/new_api_tools"
	updaterContainerName = "newapi-tools"
)
