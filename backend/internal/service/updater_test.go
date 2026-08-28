package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/new-api-tools/backend/internal/config"
)

func newUpdaterForTest(serverURL, token string) *UpdaterService {
	return &UpdaterService{
		cfg: &config.Config{
			WatchtowerToken:  token,
			WatchtowerAPIURL: serverURL,
		},
		httpClient: http.DefaultClient,
	}
}

func TestUpdaterCheckTargetsContainerAndAuthenticates(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"containers":[{"name":"newapi-tools","image":"ghcr.io/chinatoyhunter/new_api_tools:latest","digest":"sha256:old","latest_digest":"sha256:new","update_available":true}],"count":1}`))
	}))
	defer server.Close()

	result, err := newUpdaterForTest(server.URL, "shared-secret").Check(context.Background())
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if gotPath != "/v1/check" || gotQuery != "container=newapi-tools" {
		t.Fatalf("unexpected request target: %s?%s", gotPath, gotQuery)
	}
	if gotAuth != "Bearer shared-secret" {
		t.Fatalf("unexpected Authorization header: %q", gotAuth)
	}
	if available, _ := result["update_available"].(bool); !available {
		t.Fatalf("expected update_available=true, got %#v", result)
	}
}

func TestUpdaterCheckFailsClosedOnContainerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"containers":[{"name":"newapi-tools","error":"registry denied"}],"count":1}`))
	}))
	defer server.Close()

	_, err := newUpdaterForTest(server.URL, "shared-secret").Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "registry denied") {
		t.Fatalf("expected per-container registry error, got %v", err)
	}
}

func TestUpdaterCheckFailsClosedWhenTargetOutsideScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"containers":[],"count":0}`))
	}))
	defer server.Close()

	_, err := newUpdaterForTest(server.URL, "shared-secret").Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "outside Watchtower check scope") {
		t.Fatalf("expected scope error, got %v", err)
	}
}

func TestUpdaterRunUsesTargetedAsyncRequest(t *testing.T) {
	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	result, err := newUpdaterForTest(server.URL, "shared-secret").Run(context.Background(), true)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if gotPath != "/v1/update" || !strings.Contains(gotQuery, "container=newapi-tools") ||
		!strings.Contains(gotQuery, "timeout=5m") || !strings.Contains(gotQuery, "async=true") {
		t.Fatalf("unexpected update request: %s?%s", gotPath, gotQuery)
	}
	if code, _ := result["status_code"].(int); code != http.StatusAccepted {
		t.Fatalf("expected upstream status 202, got %#v", result)
	}
}

func TestUpdaterTargetContainerCannotBeOverridden(t *testing.T) {
	var gotContainer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContainer = r.URL.Query().Get("container")
		_, _ = w.Write([]byte(`{"containers":[{"name":"newapi-tools","update_available":false}],"count":1}`))
	}))
	defer server.Close()

	updater := newUpdaterForTest(server.URL, "shared-secret")
	if _, err := updater.Check(context.Background()); err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if gotContainer != updaterContainerName {
		t.Fatalf("expected fixed target %q, got %q", updaterContainerName, gotContainer)
	}
}

func TestUpdaterRequiresTokenWithoutNetworkCall(t *testing.T) {
	updater := newUpdaterForTest("http://127.0.0.1:1", "")
	if _, err := updater.Check(context.Background()); err != ErrUpdaterNotConfigured {
		t.Fatalf("Check expected ErrUpdaterNotConfigured, got %v", err)
	}
	if _, err := updater.Run(context.Background(), true); err != ErrUpdaterNotConfigured {
		t.Fatalf("Run expected ErrUpdaterNotConfigured, got %v", err)
	}
}
