package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CoolBanHub/pm/internal/config"
	"github.com/CoolBanHub/pm/internal/control"
	"github.com/CoolBanHub/pm/internal/supervisor"
)

type fakeBackend struct {
	mu         sync.Mutex
	configPath string
	requests   []control.Request
	statuses   []supervisor.Status
	events     []supervisor.Event
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
	if f.events != nil {
		return f.events
	}
	return []supervisor.Event{{ID: 1, Program: "api", Type: "started"}}
}

func (f *fakeBackend) ConfigPath() string { return f.configPath }

func TestLoginSessionAuthenticationAndOriginProtection(t *testing.T) {
	backend := &fakeBackend{statuses: []supervisor.Status{{Name: "api", State: supervisor.StateRunning, PID: os.Getpid()}}}
	server := NewServer("", "admin", "secret", backend, log.New(io.Discard, "", 0))
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/api/v1/processes")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", response.StatusCode)
	}

	if status, _ := login(t, httpServer.URL, "admin", "wrong"); status != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d", status)
	}

	status, token := login(t, httpServer.URL, "admin", "secret")
	if status != http.StatusOK || token == "" {
		t.Fatalf("login = %d token=%q", status, token)
	}

	request, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/api/v1/processes/api/restart", nil)
	request.Header.Set("Authorization", "Bearer "+token)
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
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Origin", httpServer.URL)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("same-origin status = %d", response.StatusCode)
	}

	request, _ = http.NewRequest(http.MethodPost, httpServer.URL+"/api/v1/logout", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Origin", httpServer.URL)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d", response.StatusCode)
	}

	request, _ = http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/processes", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-logout status = %d", response.StatusCode)
	}
}

func login(t *testing.T, base, username, password string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	response, err := http.Post(base+"/api/v1/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload struct {
		Token string `json:"token"`
	}
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
	}
	return response.StatusCode, payload.Token
}

func TestSessionProbeAndExpiry(t *testing.T) {
	server := NewServer("", "admin", "secret", &fakeBackend{}, log.New(io.Discard, "", 0))
	server.sessionTTL = -time.Second
	handler := server.routes()

	request := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"auth_required":true`) {
		t.Fatalf("session probe = %d %s", recorder.Code, recorder.Body.String())
	}

	loginRequest := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(`{"username":"admin","password":"secret"}`))
	loginRequest.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, loginRequest)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login = %d %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}

	apiRequest := httptest.NewRequest(http.MethodGet, "/api/v1/processes", nil)
	apiRequest.Header.Set("Authorization", "Bearer "+payload.Token)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, apiRequest)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expired session status = %d", recorder.Code)
	}
}

func TestLoginRejectedWithoutConfiguredCredentials(t *testing.T) {
	server := NewServer("", "", "", &fakeBackend{}, log.New(io.Discard, "", 0))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(`{"username":"admin","password":"secret"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("login without credentials = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestStaticAssetsUseContentFingerprintAndCachePolicy(t *testing.T) {
	server := NewServer("", "", "", &fakeBackend{}, log.New(io.Discard, "", 0))
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
	server := NewServer("", "", "", backend, log.New(io.Discard, "", 0))
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

func TestDeleteProcessWithoutPausingFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pm.yaml")
	content := "web:\n  enabled: false\nprograms:\n  - name: worker\n    command: /bin/true\n  - name: other\n    command: /bin/true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{
		configPath: path,
		statuses:   []supervisor.Status{{Name: "worker", State: supervisor.StateRunning, PID: os.Getpid()}},
	}
	server := NewServer("", "", "", backend, log.New(io.Discard, "", 0))
	handler := server.routes()

	request := httptest.NewRequest(http.MethodDelete, "/api/v1/processes/worker", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("delete running process = %d %s", response.Code, response.Body.String())
	}

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(current), "name: worker") || !strings.Contains(string(current), "name: other") {
		t.Fatalf("configuration after delete: %q", current)
	}
	backend.mu.Lock()
	reloaded := false
	for _, executed := range backend.requests {
		if executed.Action == "reload" {
			reloaded = true
		}
	}
	backend.mu.Unlock()
	if !reloaded {
		t.Fatalf("expected reload after delete, requests = %+v", backend.requests)
	}

	request = httptest.NewRequest(http.MethodDelete, "/api/v1/processes/missing", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("delete unknown process = %d %s", response.Code, response.Body.String())
	}
}

func TestPauseAndResumeProcessActions(t *testing.T) {
	backend := &fakeBackend{statuses: []supervisor.Status{{Name: "worker"}}}
	server := NewServer("", "", "", backend, log.New(io.Discard, "", 0))
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
	dir := t.TempDir()
	stdoutPath := filepath.Join(dir, "app.log")
	stderrPath := filepath.Join(dir, "app.error.log")
	if err := os.WriteFile(stdoutPath, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrPath, []byte("stderr-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{statuses: []supervisor.Status{{Name: "api", State: supervisor.StateRunning, StdoutLog: stdoutPath, StderrLog: stderrPath}}}
	server := NewServer("", "", "", backend, log.New(io.Discard, "", 0))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/logs/api?tail=2", nil)
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "two\\nthree") {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/logs/api?stream=stderr", nil)
	recorder = httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "stderr-only") || strings.Contains(recorder.Body.String(), "three") {
		t.Fatalf("stderr response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestServerStopsWithContext(t *testing.T) {
	backend := &fakeBackend{}
	server := NewServer("127.0.0.1:0", "", "", backend, log.New(io.Discard, "", 0))
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
	server := NewServer("", "", "", backend, log.New(io.Discard, "", 0))
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
	server := NewServer("", "", "", backend, log.New(io.Discard, "", 0))
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
	server := NewServer("", "", "", backend, log.New(io.Discard, "", 0))
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

func TestResetRestartsSupportsSingleAndBatchActions(t *testing.T) {
	backend := &fakeBackend{}
	server := NewServer("", "", "", backend, log.New(io.Discard, "", 0))
	handler := server.routes()

	single := httptest.NewRequest(http.MethodPost, "/api/v1/processes/api/reset-restarts", nil)
	singleResponse := httptest.NewRecorder()
	handler.ServeHTTP(singleResponse, single)
	if singleResponse.Code != http.StatusOK {
		t.Fatalf("single reset = %d %s", singleResponse.Code, singleResponse.Body.String())
	}

	batch := httptest.NewRequest(http.MethodPost, "/api/v1/actions/reset-restarts", strings.NewReader(`{"names":["api","worker"]}`))
	batch.Header.Set("Content-Type", "application/json")
	batchResponse := httptest.NewRecorder()
	handler.ServeHTTP(batchResponse, batch)
	if batchResponse.Code != http.StatusOK {
		t.Fatalf("batch reset = %d %s", batchResponse.Code, batchResponse.Body.String())
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.requests) != 2 || backend.requests[0].Action != "reset-restarts" || len(backend.requests[1].Names) != 2 {
		t.Fatalf("reset requests = %+v", backend.requests)
	}
}

func TestEventsCanBeFilteredByProgramAndType(t *testing.T) {
	backend := &fakeBackend{events: []supervisor.Event{
		{ID: 3, Program: "worker", Type: "started"},
		{ID: 2, Program: "api", Type: "restart", Restart: &supervisor.RestartSnapshot{Trigger: "manual"}},
		{ID: 1, Program: "worker", Type: "restart", Restart: &supervisor.RestartSnapshot{Trigger: "automatic"}},
	}}
	server := NewServer("", "", "", backend, log.New(io.Discard, "", 0))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events?program=worker&type=restart&limit=10", nil)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("filtered events = %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Events []supervisor.Event `json:"events"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 1 || body.Events[0].ID != 1 {
		t.Fatalf("filtered events = %+v", body.Events)
	}
}
