package control

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"
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
	response = callForTest(t, socket, Request{Action: "reset-restarts", Names: []string{"worker"}})
	if !response.OK {
		t.Fatal(response.Message)
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

func TestReloadStopsRemovedProgramWithoutBlockingStatus(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "pm.yaml")
	readyPath := filepath.Join(dir, "ready")
	cfg := config.Config{
		Socket: filepath.Join(dir, "pm.sock"), StateDir: filepath.Join(dir, ".pm"), EventHistory: 1000,
		Web: config.Web{Enabled: false}, Programs: []config.Program{stubbornProgram(readyPath)},
	}
	writeControlConfig(t, configPath, cfg)
	manager := supervisor.New(cfg.Programs)
	defer manager.StopAll()
	server := NewServer(cfg, configPath, manager, func() {}, log.New(io.Discard, "", 0))
	if response := server.Execute(Request{Action: "start", Names: []string{"stubborn"}}); !response.OK {
		t.Fatal(response.Message)
	}
	waitForFileContent(t, readyPath, "")
	before, err := manager.Status([]string{"stubborn"})
	if err != nil {
		t.Fatal(err)
	}

	cfg.Programs = nil
	writeControlConfig(t, configPath, cfg)
	reloaded := make(chan Response, 1)
	go func() { reloaded <- server.Execute(Request{Action: "reload"}) }()
	// The program leaves the managed set as soon as the reload applies, while
	// its shutdown is still pending.
	deadline := time.Now().Add(time.Second)
	for server.Execute(Request{Action: "status", Names: []string{"stubborn"}}).OK {
		if time.Now().After(deadline) {
			t.Fatal("reload did not remove the program")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case response := <-reloaded:
		t.Fatalf("reload finished before the stop timeout elapsed: %+v", response)
	default:
	}
	started := time.Now()
	response := server.Execute(Request{Action: "status"})
	if elapsed := time.Since(started); !response.OK || elapsed > 500*time.Millisecond {
		t.Fatalf("status took %s while a removed program was stopping: %+v", elapsed, response)
	}

	select {
	case response := <-reloaded:
		if !response.OK {
			t.Fatal(response.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reload did not finish")
	}
	if err := syscall.Kill(before[0].PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("removed program survived the reload: %v", err)
	}
}

func TestStatusStaysResponsiveDuringSlowRestart(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "pm.yaml")
	readyPath := filepath.Join(dir, "ready")
	cfg := config.Config{
		Socket: filepath.Join(dir, "pm.sock"), StateDir: filepath.Join(dir, ".pm"), EventHistory: 1000,
		Web: config.Web{Enabled: false}, Programs: []config.Program{stubbornProgram(readyPath)},
	}
	writeControlConfig(t, configPath, cfg)
	manager := supervisor.New(cfg.Programs)
	defer manager.StopAll()
	server := NewServer(cfg, configPath, manager, func() {}, log.New(io.Discard, "", 0))
	if response := server.Execute(Request{Action: "start", Names: []string{"stubborn"}}); !response.OK {
		t.Fatal(response.Message)
	}
	waitForFileContent(t, readyPath, "")
	before, err := manager.Status([]string{"stubborn"})
	if err != nil {
		t.Fatal(err)
	}

	restarted := make(chan Response, 1)
	go func() { restarted <- server.Execute(Request{Action: "restart", Names: []string{"stubborn"}}) }()
	// Status must observe the restart in progress instead of waiting for it.
	deadline := time.Now().Add(time.Second)
	for {
		response := server.Execute(Request{Action: "status", Names: []string{"stubborn"}})
		if !response.OK {
			t.Fatalf("unexpected status response: %+v", response)
		}
		if response.Processes[0].State == supervisor.StateStopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status never observed the restart stopping the program")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case response := <-restarted:
		t.Fatalf("restart finished before the stop timeout elapsed: %+v", response)
	default:
	}
	started := time.Now()
	response := server.Execute(Request{Action: "status"})
	if elapsed := time.Since(started); !response.OK || len(response.Processes) != 1 || elapsed > 500*time.Millisecond {
		t.Fatalf("status took %s during a slow restart: %+v", elapsed, response)
	}

	select {
	case response := <-restarted:
		if !response.OK {
			t.Fatal(response.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restart did not finish")
	}
	after, err := manager.Status([]string{"stubborn"})
	if err != nil || after[0].State != supervisor.StateRunning || after[0].PID == before[0].PID {
		t.Fatalf("program was not restarted: before=%+v after=%+v err=%v", before[0], after[0], err)
	}
}

func TestActionsDoNotWaitForSlowRestart(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "pm.yaml")
	readyPath := filepath.Join(dir, "ready")
	quick := config.DefaultProgram()
	quick.Name = "quick"
	quick.Command = "/bin/sh"
	quick.Args = []string{"-c", "sleep 30"}
	quick.Restart = "never"
	cfg := config.Config{
		Socket: filepath.Join(dir, "pm.sock"), StateDir: filepath.Join(dir, ".pm"), EventHistory: 1000,
		Web: config.Web{Enabled: false}, Programs: []config.Program{stubbornProgram(readyPath), quick},
	}
	writeControlConfig(t, configPath, cfg)
	manager := supervisor.New(cfg.Programs)
	defer manager.StopAll()
	server := NewServer(cfg, configPath, manager, func() {}, log.New(io.Discard, "", 0))
	if response := server.Execute(Request{Action: "start", Names: []string{"stubborn", "quick"}}); !response.OK {
		t.Fatal(response.Message)
	}
	waitForFileContent(t, readyPath, "")

	restarted := make(chan Response, 1)
	go func() { restarted <- server.Execute(Request{Action: "restart", Names: []string{"stubborn"}}) }()
	deadline := time.Now().Add(time.Second)
	for {
		response := server.Execute(Request{Action: "status", Names: []string{"stubborn"}})
		if !response.OK {
			t.Fatalf("unexpected status response: %+v", response)
		}
		if response.Processes[0].State == supervisor.StateStopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restart never began stopping the program")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Another program must not queue behind the restart's slow shutdown.
	started := time.Now()
	response := server.Execute(Request{Action: "stop", Names: []string{"quick"}})
	if elapsed := time.Since(started); !response.OK || elapsed > 500*time.Millisecond {
		t.Fatalf("stopping another program took %s during a slow restart: %+v", elapsed, response)
	}
	select {
	case response := <-restarted:
		t.Fatalf("restart finished before the stop timeout elapsed: %+v", response)
	default:
	}
	select {
	case response := <-restarted:
		if !response.OK {
			t.Fatal(response.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restart did not finish")
	}
	// Status lists programs by name: quick, then stubborn.
	if statuses, err := manager.Status(nil); err != nil || len(statuses) != 2 || statuses[0].Name != "quick" || statuses[0].State != supervisor.StateStopped || statuses[1].State != supervisor.StateRunning {
		t.Fatalf("unexpected final states: %+v, err = %v", statuses, err)
	}
}

// stubbornProgram ignores SIGTERM and, once its trap is installed, creates an
// empty readyPath so a test never signals it before it is ready to ignore that.
func stubbornProgram(readyPath string) config.Program {
	program := config.DefaultProgram()
	program.Name = "stubborn"
	program.Command = "/bin/sh"
	program.Args = []string{"-c", `trap "" TERM; : > "$READY_PATH"; while true; do sleep 1; done`}
	program.Environment = map[string]string{"READY_PATH": readyPath}
	program.Restart = "never"
	program.StopTimeout = "1s"
	return program
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
