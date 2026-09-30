package supervisor

import (
	"os"
	"path/filepath"
	"strings"
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

func TestAutomaticRestartRecordsExitAndLogSnapshot(t *testing.T) {
	dir := t.TempDir()
	program := testProgram("failing", "printf stdout-before-restart; printf stderr-before-restart >&2; exit 7")
	program.Restart = "unexpected"
	program.MaxRestarts = 1
	program.StdoutLog = filepath.Join(dir, "stdout.log")
	program.StderrLog = filepath.Join(dir, "stderr.log")
	events, err := NewEventStore(filepath.Join(dir, "events.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	p := newProcess(program, events)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return p.Status().State == StateFatal })

	var restart *Event
	for _, event := range events.List(0, 100) {
		if event.Type == "restart" {
			restart = &event
			break
		}
	}
	if restart == nil || restart.Restart == nil {
		t.Fatalf("restart event missing: %+v", events.List(0, 100))
	}
	snapshot := restart.Restart
	if snapshot.Trigger != "automatic" || snapshot.PreviousPID == 0 || snapshot.ExitCode == nil || *snapshot.ExitCode != 7 {
		t.Fatalf("restart snapshot = %+v", snapshot)
	}
	if !strings.Contains(snapshot.StdoutTail, "stdout-before-restart") || !strings.Contains(snapshot.StderrTail, "stderr-before-restart") {
		t.Fatalf("restart log snapshot = stdout %q, stderr %q", snapshot.StdoutTail, snapshot.StderrTail)
	}
}

func TestManualRestartRecordsSnapshot(t *testing.T) {
	dir := t.TempDir()
	program := testProgram("worker", "printf ready; printf warning-before-restart >&2; sleep 30")
	program.StdoutLog = filepath.Join(dir, "stdout.log")
	program.StderrLog = filepath.Join(dir, "stderr.log")
	events, err := NewEventStore("", 100)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewWithEvents([]config.Program{program}, events)
	defer manager.StopAll()
	if err := manager.Start([]string{"worker"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		data, err := os.ReadFile(program.StderrLog)
		return err == nil && strings.Contains(string(data), "warning-before-restart")
	})
	before, _ := manager.Status([]string{"worker"})
	if err := manager.Restart([]string{"worker"}); err != nil {
		t.Fatal(err)
	}

	eventsList := events.List(0, 100)
	var restart *Event
	for _, event := range eventsList {
		if event.Type == "restart" {
			restart = &event
			break
		}
	}
	if restart == nil || restart.Restart == nil {
		t.Fatalf("restart event missing: %+v", eventsList)
	}
	if restart.Restart.Trigger != "manual" || restart.Restart.PreviousPID != before[0].PID || !strings.Contains(restart.Restart.StderrTail, "warning-before-restart") {
		t.Fatalf("manual restart snapshot = %+v", restart.Restart)
	}
}

func TestManagerResetRestartsSupportsMultipleProcesses(t *testing.T) {
	manager := New([]config.Program{testProgram("first", "sleep 30"), testProgram("second", "sleep 30")})
	for _, process := range manager.processes {
		process.restarts = 4
		process.restartRuns = []time.Time{time.Now()}
	}
	if err := manager.ResetRestarts([]string{"first", "second"}); err != nil {
		t.Fatal(err)
	}
	statuses, err := manager.Status(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.Restarts != 0 || len(manager.processes[status.Name].restartRuns) != 0 {
			t.Fatalf("restart state was not reset for %s: %+v", status.Name, status)
		}
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

func TestProcessSeparatesStdoutAndStderr(t *testing.T) {
	dir := t.TempDir()
	program := testProgram("logger", "printf stdout-message; printf stderr-message >&2")
	program.StdoutLog = filepath.Join(dir, "stdout.log")
	program.StderrLog = filepath.Join(dir, "stderr.log")
	p := NewProcess(program)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return p.Status().State == StateExited })
	stdout, err := os.ReadFile(program.StdoutLog)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(program.StderrLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdout) != "stdout-message" || string(stderr) != "stderr-message" {
		t.Fatalf("stdout = %q, stderr = %q", stdout, stderr)
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
	first.PprofURL = "http://127.0.0.1:6060/debug/pprof"
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
			if status.PID != pids["first"] || status.Group != "critical" || status.PprofURL != first.PprofURL {
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
