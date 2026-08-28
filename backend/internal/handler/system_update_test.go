package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/new-api-tools/backend/internal/config"
	"github.com/new-api-tools/backend/internal/version"
)

func setUpdaterConfig(t *testing.T, token, apiURL string) {
	t.Helper()
	cfg := config.Get()
	oldToken := cfg.WatchtowerToken
	oldAPIURL := cfg.WatchtowerAPIURL
	oldGitHubToken := cfg.GitHubToken
	cfg.WatchtowerToken = token
	cfg.WatchtowerAPIURL = apiURL
	cfg.GitHubToken = ""

	t.Cleanup(func() {
		cfg.WatchtowerToken = oldToken
		cfg.WatchtowerAPIURL = oldAPIURL
		cfg.GitHubToken = oldGitHubToken
	})
}

func setUpdaterToken(t *testing.T, token string) {
	t.Helper()
	setUpdaterConfig(t, token, config.Get().WatchtowerAPIURL)
}

func TestGetUpdateStatusReturnsConfiguredPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	config.Load() // ensure global config exists for Status()/config.Get()
	setUpdaterToken(t, "test-token")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/system/update/status", nil)

	GetUpdateStatus(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Configured     bool   `json:"configured"`
			CurrentVersion string `json:"current_version"`
			ContainerName  string `json:"container_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !resp.Success || !resp.Data.Configured {
		t.Fatalf("expected configured=true, got %s", w.Body.String())
	}
	if resp.Data.ContainerName == "" {
		t.Fatalf("expected non-empty container_name, got %s", w.Body.String())
	}
}

func TestHealthCheckReportsBuildVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldVersion := version.GitCommit
	version.GitCommit = "abc123def456"
	t.Cleanup(func() { version.GitCommit = oldVersion })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/health", nil)

	HealthCheck(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"version":"abc123def456"`) {
		t.Fatalf("expected injected build version, got %s", w.Body.String())
	}
}

func TestCheckUpdateNotConfiguredReturnsError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	config.Load()
	setUpdaterToken(t, "")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/system/update/check", nil)

	CheckUpdate(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d: %s", http.StatusServiceUnavailable, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "UPDATER_NOT_CONFIGURED") {
		t.Fatalf("expected UPDATER_NOT_CONFIGURED, got %s", w.Body.String())
	}
}

func TestRunUpdateNoUpdateReturnsConflictWithoutTrigger(t *testing.T) {
	gin.SetMode(gin.TestMode)
	config.Load()

	var updateCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/check":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"containers":[{"name":"newapi-tools","update_available":false}],"count":1}`))
		case "/v1/update":
			updateCalls.Add(1)
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	setUpdaterConfig(t, "test-token", server.URL)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/system/update/run", nil)

	RunUpdate(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "NO_UPDATE_AVAILABLE") {
		t.Fatalf("expected NO_UPDATE_AVAILABLE, got %s", w.Body.String())
	}
	if got := updateCalls.Load(); got != 0 {
		t.Fatalf("expected no /v1/update call, got %d", got)
	}
}

func TestRunUpdateRechecksBeforeAsyncTrigger(t *testing.T) {
	gin.SetMode(gin.TestMode)
	config.Load()

	var checkCalls, updateCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/check":
			checkCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"containers":[{"name":"newapi-tools","update_available":true}],"count":1}`))
		case "/v1/update":
			updateCalls.Add(1)
			if r.URL.Query().Get("container") != "newapi-tools" || r.URL.Query().Get("async") != "true" {
				t.Errorf("unexpected update query: %s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	setUpdaterConfig(t, "test-token", server.URL)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/system/update/run?async=false", nil)

	RunUpdate(c)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if checkCalls.Load() != 1 || updateCalls.Load() != 1 {
		t.Fatalf("expected one check and one update call, got check=%d update=%d", checkCalls.Load(), updateCalls.Load())
	}
}

func TestRunUpdateNotConfiguredReturnsError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	config.Load()
	setUpdaterToken(t, "")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/system/update/run?async=false", nil)

	RunUpdate(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d: %s", http.StatusServiceUnavailable, w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "UPDATER_NOT_CONFIGURED") {
		t.Fatalf("expected UPDATER_NOT_CONFIGURED, got %s", w.Body.String())
	}
}
