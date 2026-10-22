package web

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CoolBanHub/pm/internal/config"
	"github.com/CoolBanHub/pm/internal/supervisor"
)

// writeIncludeTree creates a main config that includes a conf.d drop-in.
func writeIncludeTree(t *testing.T, dir string) (string, string) {
	t.Helper()
	main := filepath.Join(dir, "pm.yaml")
	dropIn := filepath.Join(dir, "conf.d", "web.yaml")
	if err := os.MkdirAll(filepath.Dir(dropIn), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte("web:\n  enabled: false\ninclude:\n  - conf.d/*.yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dropIn, []byte("programs:\n  - name: included-app\n    command: /bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return main, dropIn
}

func TestUpdateProgramsRejectsEditingIncludeOwnedProgram(t *testing.T) {
	dir := t.TempDir()
	main, dropIn := writeIncludeTree(t, dir)
	backend := &fakeBackend{configPath: main}
	server := NewServer("", "", "", backend, log.New(io.Discard, "", 0))
	handler := server.routes()

	body, _ := json.Marshal(map[string]any{
		"name": "included-app", "command": "/bin/other", "group": "default",
		"autostart": true, "restart": "never", "restart_delay": "1s",
		"max_restarts": 5, "restart_window": "1m", "start_secs": "0s",
		"stop_signal": "TERM", "stop_timeout": "10s",
		"log_max_bytes": 10485760, "log_backups": 3,
	})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/processes/included-app", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), dropIn) {
		t.Fatalf("error must point at the owning file: %s", response.Body.String())
	}
	if _, err := os.Stat(dropIn); err != nil {
		t.Fatalf("drop-in must be untouched: %v", err)
	}
}

func TestUpdateProgramsStillEditsMainFilePrograms(t *testing.T) {
	dir := t.TempDir()
	main, dropIn := writeIncludeTree(t, dir)
	if err := os.WriteFile(main, []byte("web:\n  enabled: false\ninclude:\n  - conf.d/*.yaml\nprograms:\n  - name: main-app\n    command: /bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{configPath: main}
	server := NewServer("", "", "", backend, log.New(io.Discard, "", 0))

	err := server.updatePrograms(func(programs []config.Program) ([]config.Program, error) {
		for i, program := range programs {
			if program.Name == "main-app" {
				programs[i].Group = "renamed"
			}
		}
		return programs, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(main)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Programs) != 2 {
		t.Fatalf("programs = %+v", cfg.Programs)
	}
	for _, program := range cfg.Programs {
		if program.Name == "main-app" && program.Group != "renamed" {
			t.Fatalf("main program edit not applied: %+v", program)
		}
		if program.Name == "included-app" && program.Source != dropIn {
			t.Fatalf("included program lost provenance: %+v", program)
		}
	}
}

func TestDeleteProcessRejectsIncludeOwnedProgram(t *testing.T) {
	dir := t.TempDir()
	main, _ := writeIncludeTree(t, dir)
	backend := &fakeBackend{
		configPath: main,
		statuses:   []supervisor.Status{{Name: "included-app", State: supervisor.StateStopped}},
	}
	server := NewServer("", "", "", backend, log.New(io.Discard, "", 0))
	handler := server.routes()

	request := httptest.NewRequest(http.MethodDelete, "/api/v1/processes/included-app", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	for _, request := range backend.requests {
		if request.Action == "reload" {
			t.Fatal("delete of an include-owned program must not trigger reload")
		}
	}
}
