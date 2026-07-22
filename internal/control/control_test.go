package control

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CoolBanHub/pm/internal/config"
	"github.com/CoolBanHub/pm/internal/supervisor"
	"gopkg.in/yaml.v3"
)

func TestServerLifecycle(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "pm.sock")
	configPath := filepath.Join(dir, "pm.yaml")
	program := config.DefaultProgram()
	program.Name = "worker"
	program.Command = "/bin/sh"
	program.Args = []string{"-c", "sleep 30"}
	program.Restart = "never"
	program.RestartDelay = "10ms"
	program.MaxRestarts = 2
	program.RestartWindow = "1s"
	program.StopTimeout = "500ms"
	states, err := supervisor.NewProgramStateStore(filepath.Join(dir, supervisor.ProgramStateFile))
	if err != nil {
		t.Fatal(err)
	}
	manager := supervisor.NewWithState([]config.Program{program}, nil, states)
	ctx, cancel := context.WithCancel(context.Background())
	cfg := config.Config{Socket: socket, StateDir: filepath.Join(dir, ".pm"), EventHistory: 1000, Web: config.Web{Enabled: false}, Programs: []config.Program{program}}
	writeControlConfig(t, configPath, cfg)
	server := NewServer(cfg, configPath, manager, cancel, log.New(io.Discard, "", 0))
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForSocket(t, socket)

	response := callForTest(t, socket, Request{Action: "start", Names: []string{"worker"}})
	if !response.OK {
		t.Fatal(response.Message)
	}
	response = callForTest(t, socket, Request{Action: "status", Names: []string{"worker"}})
	if !response.OK || len(response.Processes) != 1 || response.Processes[0].State != supervisor.StateRunning {
		t.Fatalf("unexpected status response: %+v", response)
	}
	firstPID := response.Processes[0].PID
	response = callForTest(t, socket, Request{Action: "restart", Names: []string{"worker"}})
	if !response.OK {
		t.Fatal(response.Message)
	}
	response = callForTest(t, socket, Request{Action: "status", Names: []string{"worker"}})
	if !response.OK || len(response.Processes) != 1 || response.Processes[0].State != supervisor.StateRunning || response.Processes[0].PID == firstPID {
		t.Fatalf("unexpected restarted status: %+v", response)
	}
	response = callForTest(t, socket, Request{Action: "pause", Names: []string{"worker"}})
	if !response.OK {
		t.Fatal(response.Message)
	}
	response = callForTest(t, socket, Request{Action: "status", Names: []string{"worker"}})
	if !response.OK || !response.Processes[0].Paused || response.Processes[0].State != supervisor.StateStopped {
		t.Fatalf("unexpected paused status: %+v", response)
	}
	response = callForTest(t, socket, Request{Action: "resume", Names: []string{"worker"}})
	if !response.OK {
		t.Fatal(response.Message)
	}
	response = callForTest(t, socket, Request{Action: "stop", Names: []string{"worker"}})
	if !response.OK {
		t.Fatal(response.Message)
	}
	response = callForTest(t, socket, Request{Action: "shutdown"})
	if !response.OK {
		t.Fatal(response.Message)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestRestartUsesLatestProgramEnvironment(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "environment.txt")
	program := config.DefaultProgram()
	program.Name = "worker"
	program.Command = "/bin/sh"
	program.Args = []string{"-c", `printf %s "$RESTART_VALUE" > "$RESULT_PATH"; sleep 30`}
	program.Environment = map[string]string{"RESTART_VALUE": "before", "RESULT_PATH": resultPath}
	program.Restart = "never"
	program.StopTimeout = "500ms"
	cfg := config.Config{
		Socket: filepath.Join(dir, "pm.sock"), StateDir: filepath.Join(dir, ".pm"), EventHistory: 1000,
		Web: config.Web{Enabled: false}, Programs: []config.Program{program},
	}
	configPath := filepath.Join(dir, "pm.yaml")
	writeControlConfig(t, configPath, cfg)
	manager := supervisor.New(cfg.Programs)
	defer manager.StopAll()
	server := NewServer(cfg, configPath, manager, func() {}, log.New(io.Discard, "", 0))

	if response := server.Execute(Request{Action: "start", Names: []string{"worker"}}); !response.OK {
		t.Fatal(response.Message)
	}
	waitForFileContent(t, resultPath, "before")
	before, err := manager.Status([]string{"worker"})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(configPath, []byte("programs: [invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if response := server.Execute(Request{Action: "restart", Names: []string{"worker"}}); response.OK {
		t.Fatal("restart accepted an invalid configuration")
	}
	unchanged, err := manager.Status([]string{"worker"})
	if err != nil {
		t.Fatal(err)
	}
	if unchanged[0].PID != before[0].PID || unchanged[0].State != supervisor.StateRunning {
		t.Fatalf("invalid configuration disturbed the process: before=%+v after=%+v", before[0], unchanged[0])
	}

	cfg.Programs[0].Environment["RESTART_VALUE"] = "after"
	writeControlConfig(t, configPath, cfg)
	if response := server.Execute(Request{Action: "restart", Names: []string{"worker"}}); !response.OK {
		t.Fatal(response.Message)
	}
	waitForFileContent(t, resultPath, "after")
	after, err := manager.Status([]string{"worker"})
	if err != nil {
		t.Fatal(err)
	}
	if after[0].PID == 0 || after[0].PID == before[0].PID {
		t.Fatalf("process was not restarted: before=%+v after=%+v", before[0], after[0])
	}
}

func writeControlConfig(t *testing.T, path string, cfg config.Config) {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForFileContent(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && string(data) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	data, err := os.ReadFile(path)
	t.Fatalf("file content = %q, err = %v, want %q", data, err, want)
}

func callForTest(t *testing.T, socket string, request Request) Response {
	t.Helper()
	response, err := Call(socket, request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func waitForSocket(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if response, err := Call(socket, Request{Action: "status"}); err == nil && response.OK {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("socket did not become ready")
}
