package web

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/CoolBanHub/pm/internal/config"
	"github.com/CoolBanHub/pm/internal/control"
	"github.com/CoolBanHub/pm/internal/supervisor"
)

type fakeBackend struct {
	mu         sync.Mutex
	configPath string
	requests   []control.Request
	statuses   []supervisor.Status
}

func (f *fakeBackend) Execute(request control.Request) control.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if request.Action == "status" {
		return control.Response{OK: true, Processes: f.statuses}
	}
	return control.Response{OK: true, Message: request.Action + " complete"}
}

func (f *fakeBackend) Events(uint64, int) []supervisor.Event {
	return []supervisor.Event{{ID: 1, Program: "api", Type: "started"}}
}

func (f *fakeBackend) ConfigPath() string { return f.configPath }

func TestAPIAuthenticationAndOriginProtection(t *testing.T) {
	backend := &fakeBackend{statuses: []supervisor.Status{{Name: "api", State: supervisor.StateRunning, PID: os.Getpid()}}}
	server := NewServer("", "secret", backend, log.New(io.Discard, "", 0))
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/api/v1/session")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.StatusCode)
	}

	request, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/api/v1/processes/api/restart", nil)
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Origin", "https://attacker.example")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d", response.StatusCode)
	}

	request, _ = http.NewRequest(http.MethodPost, httpServer.URL+"/api/v1/processes/api/restart", nil)
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Origin", httpServer.URL)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("same-origin status = %d", response.StatusCode)
	}
}

func TestStaticAssetsUseContentFingerprintAndCachePolicy(t *testing.T) {
	server := NewServer("", "", &fakeBackend{}, log.New(io.Discard, "", 0))
	handler := server.routes()

	indexRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	indexResponse := httptest.NewRecorder()
	handler.ServeHTTP(indexResponse, indexRequest)
	if indexResponse.Code != http.StatusOK || indexResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("index = %d cache=%q", indexResponse.Code, indexResponse.Header().Get("Cache-Control"))
	}
	marker := `src="/assets/`
	start := strings.Index(indexResponse.Body.String(), marker)
	if start < 0 {
		t.Fatalf("fingerprinted script missing from index: %s", indexResponse.Body.String())
	}
	start += len(marker)
	end := strings.Index(indexResponse.Body.String()[start:], `/app.js"`)
	if end < 0 {
		t.Fatalf("invalid fingerprinted script path: %s", indexResponse.Body.String())
	}
	version := indexResponse.Body.String()[start : start+end]
	if len(version) != 12 {
		t.Fatalf("asset version = %q", version)
	}
	for _, name := range []string{"app.js", "app.css", "favicon.svg"} {
		if !strings.Contains(indexResponse.Body.String(), "/assets/"+version+"/"+name) {
			t.Fatalf("%s does not use version %s", name, version)
		}
	}

	assetRequest := httptest.NewRequest(http.MethodGet, "/assets/"+version+"/app.js", nil)
	assetResponse := httptest.NewRecorder()
	handler.ServeHTTP(assetResponse, assetRequest)
	if assetResponse.Code != http.StatusOK || assetResponse.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("asset = %d cache=%q", assetResponse.Code, assetResponse.Header().Get("Cache-Control"))
	}
	if !strings.Contains(assetResponse.Body.String(), "bootstrap();") {
		t.Fatal("fingerprinted app.js content is missing")
	}

	staleRequest := httptest.NewRequest(http.MethodGet, "/assets/old-version/app.js", nil)
	staleResponse := httptest.NewRecorder()
	handler.ServeHTTP(staleResponse, staleRequest)
	if staleResponse.Code != http.StatusNotFound {
		t.Fatalf("stale asset status = %d", staleResponse.Code)
	}

	legacyRequest := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	legacyResponse := httptest.NewRecorder()
	handler.ServeHTTP(legacyResponse, legacyRequest)
	if legacyResponse.Code != http.StatusOK || legacyResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("legacy asset = %d cache=%q", legacyResponse.Code, legacyResponse.Header().Get("Cache-Control"))
	}

	fallbackRequest := httptest.NewRequest(http.MethodGet, "/processes/example", nil)
	fallbackResponse := httptest.NewRecorder()
	handler.ServeHTTP(fallbackResponse, fallbackRequest)
	if fallbackResponse.Code != http.StatusOK || fallbackResponse.Header().Get("Cache-Control") != "no-store" || !strings.Contains(fallbackResponse.Body.String(), "<!doctype html>") {
		t.Fatalf("fallback = %d cache=%q body=%q", fallbackResponse.Code, fallbackResponse.Header().Get("Cache-Control"), fallbackResponse.Body.String())
	}
}

func TestConfigUpdateIsValidatedBackedUpAndApplied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pm.yaml")
	original := "web:\n  enabled: false\nprograms:\n  - name: api\n    command: /bin/true\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{configPath: path}
	server := NewServer("", "", backend, log.New(io.Discard, "", 0))
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	updated := "web:\n  enabled: false\nprograms:\n  - name: worker\n    command: /bin/true\n"
	body, _ := json.Marshal(map[string]any{"content": updated, "apply": true})
	request, _ := http.NewRequest(http.MethodPut, httpServer.URL+"/api/v1/config", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", httpServer.URL)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d: %s", response.StatusCode, data)
	}
	current, _ := os.ReadFile(path)
	backup, _ := os.ReadFile(path + ".bak")
	if string(current) != updated || string(backup) != original {
		t.Fatalf("current=%q backup=%q", current, backup)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.requests) != 1 || backend.requests[0].Action != "reload" {
		t.Fatalf("requests = %+v", backend.requests)
	}
}

func TestDeleteProcessRequiresPausedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pm.yaml")
	content := "web:\n  enabled: false\nprograms:\n  - name: worker\n    command: /bin/true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{
		configPath: path,
		statuses:   []supervisor.Status{{Name: "worker", State: supervisor.StateStopped}},
	}
	server := NewServer("", "", backend, log.New(io.Discard, "", 0))
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/processes/worker", nil)
	response := httptest.NewRecorder()

	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "must be paused") {
		t.Fatalf("delete = %d %s", response.Code, response.Body.String())
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != content {
		t.Fatalf("configuration changed after rejected delete: %q", current)
	}
}

func TestPauseAndResumeProcessActions(t *testing.T) {
	backend := &fakeBackend{statuses: []supervisor.Status{{Name: "worker"}}}
	server := NewServer("", "", backend, log.New(io.Discard, "", 0))
	handler := server.routes()
	for _, action := range []string{"pause", "resume"} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/processes/worker/"+action, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", action, response.Code, response.Body.String())
		}
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.requests) != 2 || backend.requests[0].Action != "pause" || backend.requests[1].Action != "resume" {
		t.Fatalf("requests = %+v", backend.requests)
	}
}

func TestLogTailEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{statuses: []supervisor.Status{{Name: "api", State: supervisor.StateRunning, StdoutLog: path}}}
	server := NewServer("", "", backend, log.New(io.Discard, "", 0))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/logs/api?tail=2", nil)
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "two\\nthree") {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestServerStopsWithContext(t *testing.T) {
	backend := &fakeBackend{}
	server := NewServer("127.0.0.1:0", "", backend, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.Serve(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestProcessCRUDUpdatesConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pm.yaml")
	if err := os.WriteFile(path, []byte("web:\n  enabled: false\nprograms: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{configPath: path, statuses: []supervisor.Status{{Name: "worker", State: supervisor.StateStopped, Paused: true}}}
	server := NewServer("", "", backend, log.New(io.Discard, "", 0))
	handler := server.routes()

	create := httptest.NewRequest(http.MethodPost, "/api/v1/processes", strings.NewReader(`{"name":"worker","group":"jobs","command":"/bin/true"}`))
	create.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, create)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Programs) != 1 || cfg.Programs[0].Group != "jobs" || cfg.Programs[0].Restart != "unexpected" {
		t.Fatalf("created config = %+v", cfg.Programs)
	}

	update := httptest.NewRequest(http.MethodPut, "/api/v1/processes/worker", strings.NewReader(`{"name":"worker","group":"critical","command":"/bin/sleep","args":["10"]}`))
	update.Header.Set("Content-Type", "application/json")
	updated := httptest.NewRecorder()
	handler.ServeHTTP(updated, update)
	if updated.Code != http.StatusOK {
		t.Fatalf("update = %d %s", updated.Code, updated.Body.String())
	}
	cfg, _ = config.Load(path)
	if cfg.Programs[0].Group != "critical" || cfg.Programs[0].Command != "/bin/sleep" {
		t.Fatalf("updated config = %+v", cfg.Programs[0])
	}

	remove := httptest.NewRequest(http.MethodDelete, "/api/v1/processes/worker", nil)
	removed := httptest.NewRecorder()
	handler.ServeHTTP(removed, remove)
	if removed.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", removed.Code, removed.Body.String())
	}
	cfg, _ = config.Load(path)
	if len(cfg.Programs) != 0 {
		t.Fatalf("programs after delete = %+v", cfg.Programs)
	}
}

func TestCreateProcessWritesInitiallyMissingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pm.yaml")
	backend := &fakeBackend{configPath: path}
	server := NewServer("", "", backend, log.New(io.Discard, "", 0))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/processes", strings.NewReader(`{"name":"worker","command":"/bin/true"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", response.Code, response.Body.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Programs) != 1 || cfg.Programs[0].Name != "worker" || !cfg.Web.Enabled {
		t.Fatalf("created config = %+v", cfg)
	}
}

func TestSelectedAndGroupBatchActions(t *testing.T) {
	backend := &fakeBackend{statuses: []supervisor.Status{
		{Name: "api", Group: "web", State: supervisor.StateRunning},
		{Name: "worker", Group: "jobs", State: supervisor.StateRunning},
		{Name: "scheduler", Group: "jobs", State: supervisor.StateStopped},
	}}
	server := NewServer("", "", backend, log.New(io.Discard, "", 0))
	handler := server.routes()

	selected := httptest.NewRequest(http.MethodPost, "/api/v1/actions/restart", strings.NewReader(`{"names":["api","worker"]}`))
	selected.Header.Set("Content-Type", "application/json")
	selectedResponse := httptest.NewRecorder()
	handler.ServeHTTP(selectedResponse, selected)
	if selectedResponse.Code != http.StatusOK {
		t.Fatalf("selected action = %d %s", selectedResponse.Code, selectedResponse.Body.String())
	}

	group := httptest.NewRequest(http.MethodPost, "/api/v1/groups/jobs/stop", nil)
	groupResponse := httptest.NewRecorder()
	handler.ServeHTTP(groupResponse, group)
	if groupResponse.Code != http.StatusOK {
		t.Fatalf("group action = %d %s", groupResponse.Code, groupResponse.Body.String())
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.requests) != 3 {
		t.Fatalf("requests = %+v", backend.requests)
	}
	if got := backend.requests[0].Names; len(got) != 2 || got[0] != "api" || got[1] != "worker" {
		t.Fatalf("selected names = %v", got)
	}
	if backend.requests[1].Action != "status" {
		t.Fatalf("group lookup = %+v", backend.requests[1])
	}
	if got := backend.requests[2].Names; len(got) != 2 || got[0] != "worker" || got[1] != "scheduler" {
		t.Fatalf("group names = %v", got)
	}
}
