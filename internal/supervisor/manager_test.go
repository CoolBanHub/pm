package supervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CoolBanHub/pm/internal/config"
)

func testProgram(name, script string) config.Program {
	return config.Program{
		Name: name, Command: "/bin/sh", Args: []string{"-c", script},
		Restart: "never", RestartDelay: "10ms", MaxRestarts: 2,
		RestartWindow: "1s", StopSignal: "TERM", StopTimeout: "500ms",
	}
}

func TestProcessStartAndStop(t *testing.T) {
	p := NewProcess(testProgram("sleeper", "sleep 30"))
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	if status := p.Status(); status.State != StateRunning || status.PID == 0 {
		t.Fatalf("unexpected running status: %+v", status)
	}
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	if status := p.Status(); status.State != StateStopped || status.PID != 0 {
		t.Fatalf("unexpected stopped status: %+v", status)
	}
}

func TestUnexpectedExitRestartsAndBecomesFatal(t *testing.T) {
	program := testProgram("failing", "exit 3")
	program.Restart = "unexpected"
	p := NewProcess(program)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return p.Status().State == StateFatal })
	status := p.Status()
	if status.Restarts != 2 || status.Starts != 3 {
		t.Fatalf("unexpected restart counts: %+v", status)
	}
}

func TestProcessWritesLog(t *testing.T) {
	program := testProgram("logger", "printf hello")
	program.StdoutLog = filepath.Join(t.TempDir(), "nested", "out.log")
	p := NewProcess(program)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return p.Status().State == StateExited })
	data, err := os.ReadFile(program.StdoutLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("log = %q", data)
	}
}

func TestManagerApplyOnlyRestartsChangedProcesses(t *testing.T) {
	first := testProgram("first", "sleep 30")
	first.Group = "workers"
	first.Autostart = true
	second := testProgram("second", "sleep 30")
	second.Group = "workers"
	second.Autostart = true
	manager := New([]config.Program{first, second})
	defer manager.StopAll()
	if errs := manager.Autostart(); len(errs) != 0 {
		t.Fatal(errs)
	}
	before, err := manager.Status(nil)
	if err != nil {
		t.Fatal(err)
	}
	pids := map[string]int{before[0].Name: before[0].PID, before[1].Name: before[1].PID}

	first.Group = "critical"
	third := testProgram("third", "sleep 30")
	third.Group = "jobs"
	third.Autostart = false
	if err := manager.Apply([]config.Program{first, second, third}); err != nil {
		t.Fatal(err)
	}
	after, err := manager.Status(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range after {
		switch status.Name {
		case "first":
			if status.PID != pids["first"] || status.Group != "critical" {
				t.Fatalf("metadata update restarted first: %+v", status)
			}
		case "second":
			if status.PID != pids["second"] {
				t.Fatalf("unchanged second was restarted: %+v", status)
			}
		case "third":
			if status.State != StateStopped || status.PID != 0 {
				t.Fatalf("non-autostart third is active: %+v", status)
			}
		}
	}

	first.Args = []string{"-c", "sleep 29"}
	if err := manager.Apply([]config.Program{first, third}); err != nil {
		t.Fatal(err)
	}
	statuses, err := manager.Status([]string{"first"})
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].PID == pids["first"] || statuses[0].State != StateRunning {
		t.Fatalf("changed first was not restarted: %+v", statuses[0])
	}
	if _, err := manager.Status([]string{"second"}); err == nil {
		t.Fatal("removed process is still registered")
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
