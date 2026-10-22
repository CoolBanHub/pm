package supervisor

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/CoolBanHub/pm/internal/config"
)

func TestGroupSelectionResolvesGroupTargets(t *testing.T) {
	programs := []config.Program{
		{Name: "web-a", Group: "web", Command: "/bin/sh", Args: []string{"-c", "sleep 30"}, Restart: "never", RestartDelay: "10ms", RestartWindow: "1s", StopSignal: "TERM", StopTimeout: "500ms"},
		{Name: "web-b", Group: "web", Command: "/bin/sh", Args: []string{"-c", "sleep 30"}, Restart: "never", RestartDelay: "10ms", RestartWindow: "1s", StopSignal: "TERM", StopTimeout: "500ms"},
		{Name: "db", Group: "db", Command: "/bin/sh", Args: []string{"-c", "sleep 30"}, Restart: "never", RestartDelay: "10ms", RestartWindow: "1s", StopSignal: "TERM", StopTimeout: "500ms"},
	}
	manager := New(programs)

	for _, target := range []string{"group:web", "web:*"} {
		if err := manager.Stop([]string{target}); err != nil {
			t.Fatalf("group target %q: %v", target, err)
		}
		statuses, err := manager.Status([]string{target})
		if err != nil {
			t.Fatalf("status %q: %v", target, err)
		}
		if len(statuses) != 2 {
			t.Fatalf("group %q resolved %d processes, want 2", target, len(statuses))
		}
		for _, status := range statuses {
			if status.Group != "web" {
				t.Fatalf("group %q selected %q from group %q", target, status.Name, status.Group)
			}
		}
	}

	if err := manager.Start([]string{"group:web"}); err != nil {
		t.Fatalf("start group:web: %v", err)
	}
	defer func() { _ = manager.StopAll() }()
	for _, name := range []string{"web-a", "web-b"} {
		statuses, _ := manager.Status([]string{name})
		if statuses[0].PID == 0 {
			t.Fatalf("%s was not started by group start", name)
		}
	}
	statuses, _ := manager.Status([]string{"db"})
	if statuses[0].PID != 0 {
		t.Fatal("db must not be started by group:web start")
	}

	if _, err := manager.Status([]string{"group:nosuch"}); err == nil {
		t.Fatal("unknown group must error")
	}
}

func TestGroupAndNameMixWithoutDuplicates(t *testing.T) {
	programs := []config.Program{
		{Name: "web-a", Group: "web", Command: "/bin/sh", Args: []string{"-c", "sleep 30"}, Restart: "never", RestartDelay: "10ms", RestartWindow: "1s", StopSignal: "TERM", StopTimeout: "500ms"},
		{Name: "web-b", Group: "web", Command: "/bin/sh", Args: []string{"-c", "sleep 30"}, Restart: "never", RestartDelay: "10ms", RestartWindow: "1s", StopSignal: "TERM", StopTimeout: "500ms"},
	}
	manager := New(programs)
	selected, err := manager.selectProcesses([]string{"group:web", "web-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 {
		t.Fatalf("selected %d processes, want 2 unique", len(selected))
	}
}

// chronologicalEvents returns the store's events oldest-first (List yields
// newest-first) so ordering assertions read in real time order.
func chronologicalEvents(events *EventStore) []Event {
	listed := events.List(0, 1000)
	ordered := make([]Event, 0, len(listed))
	for i := len(listed) - 1; i >= 0; i-- {
		ordered = append(ordered, listed[i])
	}
	return ordered
}

func TestStopAllOrdersByPriorityDescending(t *testing.T) {
	dir := t.TempDir()
	events, err := NewEventStore(filepath.Join(dir, "events.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	programs := []config.Program{
		{Name: "app", Group: "default", Autostart: true, Priority: 20, Command: "/bin/sh", Args: []string{"-c", "sleep 30"}, Restart: "never", RestartDelay: "10ms", RestartWindow: "1s", StopSignal: "TERM", StopTimeout: "500ms"},
		{Name: "db", Group: "default", Autostart: true, Priority: 10, Command: "/bin/sh", Args: []string{"-c", "sleep 30"}, Restart: "never", RestartDelay: "10ms", RestartWindow: "1s", StopSignal: "TERM", StopTimeout: "500ms"},
		{Name: "sidecar", Group: "default", Autostart: true, Priority: 20, Command: "/bin/sh", Args: []string{"-c", "sleep 30"}, Restart: "never", RestartDelay: "10ms", RestartWindow: "1s", StopSignal: "TERM", StopTimeout: "500ms"},
	}
	manager := NewWithEvents(programs, events)
	for _, err := range manager.Autostart() {
		t.Fatal(err)
	}
	if err := manager.StopAll(); err != nil {
		t.Fatal(err)
	}

	var stopOrder []string
	for _, event := range chronologicalEvents(events) {
		if event.Type == "stopping" {
			stopOrder = append(stopOrder, event.Program)
		}
	}
	// Highest priority stops first; equal priorities fall back to name order.
	want := []string{"app", "sidecar", "db"}
	if len(stopOrder) != len(want) {
		t.Fatalf("stop order = %v", stopOrder)
	}
	for i := range want {
		if stopOrder[i] != want[i] {
			t.Fatalf("stop order = %v, want %v", stopOrder, want)
		}
	}
}

func TestAutostartOrdersByPriorityAscending(t *testing.T) {
	dir := t.TempDir()
	events, err := NewEventStore(filepath.Join(dir, "events.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	programs := []config.Program{
		{Name: "app", Group: "default", Autostart: true, Priority: 20, Command: "/bin/sh", Args: []string{"-c", "sleep 30"}, Restart: "never", RestartDelay: "10ms", RestartWindow: "1s", StopSignal: "TERM", StopTimeout: "500ms"},
		{Name: "db", Group: "default", Autostart: true, Priority: 10, Command: "/bin/sh", Args: []string{"-c", "sleep 30"}, Restart: "never", RestartDelay: "10ms", RestartWindow: "1s", StopSignal: "TERM", StopTimeout: "500ms"},
	}
	manager := NewWithEvents(programs, events)
	for _, err := range manager.Autostart() {
		t.Fatal(err)
	}
	defer func() { _ = manager.StopAll() }()

	var startOrder []string
	for _, event := range chronologicalEvents(events) {
		if event.Type == "started" {
			startOrder = append(startOrder, event.Program)
		}
	}
	if len(startOrder) != 2 || startOrder[0] != "db" || startOrder[1] != "app" {
		t.Fatalf("start order = %v, want [db app]", startOrder)
	}
}

func TestStartSecsGateTreatsEarlyExitAsFailedStart(t *testing.T) {
	// exit 0 is an expected exit, but exiting inside the start window counts as
	// a failed start and must still restart under the "unexpected" policy.
	program := testProgram("flappy", "exit 0")
	program.Restart = "unexpected"
	program.StartSecs = "300ms"
	program.MaxRestarts = 1
	p := NewProcess(program)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return p.Status().State == StateFatal })
	status := p.Status()
	if status.Restarts != 1 {
		t.Fatalf("restarts = %d, want 1 (early exit must retry)", status.Restarts)
	}
}

func TestStartSecsExposesStartingThenRunning(t *testing.T) {
	program := testProgram("sleeper", "sleep 30")
	program.StartSecs = "300ms"
	p := NewProcess(program)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Stop() }()
	if state := p.Status().State; state != StateStarting {
		t.Fatalf("state right after start = %s, want STARTING", state)
	}
	waitFor(t, time.Second, func() bool { return p.Status().State == StateRunning })
}

func TestExpectedExitCodesSkipRestart(t *testing.T) {
	program := testProgram("clean", "exit 3")
	program.Restart = "unexpected"
	program.ExitCodes = []int{0, 3}
	p := NewProcess(program)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return p.Status().State == StateExited })
	time.Sleep(150 * time.Millisecond)
	status := p.Status()
	if status.Restarts != 0 || *status.ExitCode != 3 {
		t.Fatalf("expected clean exit without restart: %+v", status)
	}
}

func TestRuntimeChangeDetectionIgnoresPriorityAndGroup(t *testing.T) {
	base := testProgram("api", "sleep 30")
	changed := base
	changed.Priority = 5
	changed.Group = "other"
	if !runtimeEqual(base, changed) {
		t.Fatal("priority/group changes must not be treated as runtime changes")
	}
	changedRuntime := base
	changedRuntime.Args = []string{"-c", "sleep 60"}
	if runtimeEqual(base, changedRuntime) {
		t.Fatal("args change must be a runtime change")
	}
}
