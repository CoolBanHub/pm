package control

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/CoolBanHub/pm/internal/config"
	"github.com/CoolBanHub/pm/internal/supervisor"
)

func TestServerLifecycle(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "pm.sock")
	configPath := filepath.Join(dir, "pm.yaml")
	program := config.Program{
		Name: "worker", Command: "/bin/sh", Args: []string{"-c", "sleep 30"},
		Restart: "never", RestartDelay: "10ms", MaxRestarts: 2,
		RestartWindow: "1s", StopSignal: "TERM", StopTimeout: "500ms",
	}
	manager := supervisor.New([]config.Program{program})
	ctx, cancel := context.WithCancel(context.Background())
	cfg := config.Config{Socket: socket, StateDir: filepath.Join(dir, ".pm"), EventHistory: 1000, Web: config.Web{Enabled: false}}
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
