package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

// stubbornProgram is a test program that ignores SIGTERM and creates the
// returned ready file once its trap is installed, so a test can stop it
// without racing its startup.
func stubbornProgram(t *testing.T) (config.Program, string) {
	t.Helper()
	return namedStubbornProgram(t, "stubborn", "300ms")
}

// namedStubbornProgram is stubbornProgram under another name and stop timeout.
func namedStubbornProgram(t *testing.T, name, stopTimeout string) (config.Program, string) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	program := testProgram(name, `trap "" TERM; : > "`+ready+`"; while true; do sleep 1; done`)
	program.StopTimeout = stopTimeout
	return program, ready
}

// unsignalable makes the daemon's signals to pid fail with EPERM while run
// executes, as they would for a process running as another user.
func unsignalable(t *testing.T, pid int, run func()) {
	t.Helper()
	real := signalGroup
	signalGroup = func(target int, sig syscall.Signal) error {
		if target == -pid {
			return syscall.EPERM
		}
		return real(target, sig)
	}
	defer func() { signalGroup = real }()
	run()
}

func TestManagerApplyWaitsForRemovedProcessExit(t *testing.T) {
	program, ready := stubbornProgram(t)
	manager := New([]config.Program{program})
	defer manager.StopAll()
	if err := manager.Start([]string{"stubborn"}); err != nil {
		t.Fatal(err)
	}
	waitForReady(t, ready)
	before, err := manager.Status([]string{"stubborn"})
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Status([]string{"stubborn"}); err == nil {
		t.Fatal("removed process is still registered")
	}
	if err := syscall.Kill(before[0].PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("removed process survived Apply: %v", err)
	}
}

func TestManagerApplyStopsRemovedProcessesConcurrently(t *testing.T) {
	var (
		programs []config.Program
		ready    []string
	)
	for _, name := range []string{"first", "second", "third"} {
		program, file := namedStubbornProgram(t, name, "400ms")
		programs = append(programs, program)
		ready = append(ready, file)
	}
	manager := New(programs)
	defer manager.StopAll()
	if err := manager.Start(nil); err != nil {
		t.Fatal(err)
	}
	for _, file := range ready {
		waitForReady(t, file)
	}

	started := time.Now()
	if err := manager.Apply(nil); err != nil {
		t.Fatal(err)
	}
	// One after another the three shutdowns take at least 1.2s.
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Apply took %s to stop three processes with a 400ms timeout", elapsed)
	}
}

func TestManagerActionsDoNotWaitForSlowReload(t *testing.T) {
	stubborn, ready := namedStubbornProgram(t, "stubborn", "800ms")
	quick := testProgram("quick", "sleep 30")
	manager := New([]config.Program{stubborn, quick})
	defer manager.StopAll()
	if err := manager.Start(nil); err != nil {
		t.Fatal(err)
	}
	waitForReady(t, ready)

	applied := make(chan error, 1)
	go func() { applied <- manager.Apply([]config.Program{quick}) }()
	// The stubborn program leaves the table at once; only its shutdown is slow.
	waitFor(t, time.Second, func() bool {
		_, err := manager.Status([]string{"stubborn"})
		return err != nil
	})
	started := time.Now()
	if err := manager.Stop([]string{"quick"}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
		t.Fatalf("stopping another process took %s behind a slow shutdown", elapsed)
	}
	select {
	case err := <-applied:
		t.Fatalf("reload finished before the shutdown it waits for: %v", err)
	default:
	}
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	if statuses, err := manager.Status([]string{"quick"}); err != nil || statuses[0].State != StateStopped {
		t.Fatalf("quick was not stopped: %+v, err = %v", statuses, err)
	}
}

func TestManagerStopWaitsForRestartOnTheSameProcess(t *testing.T) {
	stubborn, ready := namedStubbornProgram(t, "stubborn", "800ms")
	manager := New([]config.Program{stubborn})
	defer manager.StopAll()
	if err := manager.Start([]string{"stubborn"}); err != nil {
		t.Fatal(err)
	}
	waitForReady(t, ready)

	restarted := make(chan error, 1)
	go func() { restarted <- manager.Restart([]string{"stubborn"}) }()
	waitFor(t, time.Second, func() bool {
		statuses, _ := manager.Status([]string{"stubborn"})
		return len(statuses) == 1 && statuses[0].State == StateStopping
	})
	// Changes to one process still queue behind each other, so the stop lands
	// on the process the restart brings up.
	if err := manager.Stop([]string{"stubborn"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-restarted:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("stop returned while the restart it must wait for was still running")
	}
	if statuses, err := manager.Status([]string{"stubborn"}); err != nil || statuses[0].State != StateStopped || statuses[0].PID != 0 {
		t.Fatalf("stop did not win over the restart: %+v, err = %v", statuses, err)
	}
}

func TestManagerStopAllWaitsForReloadShutdown(t *testing.T) {
	stubborn, ready := namedStubbornProgram(t, "stubborn", "800ms")
	quick := testProgram("quick", "sleep 30")
	manager := New([]config.Program{stubborn, quick})
	if err := manager.Start(nil); err != nil {
		t.Fatal(err)
	}
	waitForReady(t, ready)
	before, err := manager.Status([]string{"stubborn"})
	if err != nil {
		t.Fatal(err)
	}

	applied := make(chan error, 1)
	go func() { applied <- manager.Apply([]config.Program{quick}) }()
	waitFor(t, time.Second, func() bool {
		_, err := manager.Status([]string{"stubborn"})
		return err != nil
	})
	stopped := make(chan error, 1)
	go func() { stopped <- manager.StopAll() }()
	select {
	case err := <-stopped:
		t.Fatalf("StopAll returned while a reload's shutdown was still running: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := syscall.Kill(before[0].PID, 0); err != nil {
		t.Fatalf("shutdown finished before its stop timeout: %v", err)
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StopAll did not return")
	}
	<-applied
	if err := syscall.Kill(before[0].PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("removed process survived StopAll: %v", err)
	}
	if err := manager.Start([]string{"quick"}); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("action accepted after StopAll: %v", err)
	}
}

func TestManagerKeepsProcessListedWhenRemovalCannotStopIt(t *testing.T) {
	program := testProgram("stuck", "sleep 30")
	other := testProgram("other", "sleep 30")
	manager := New([]config.Program{program})
	defer manager.StopAll()
	if err := manager.Start([]string{"stuck"}); err != nil {
		t.Fatal(err)
	}
	before, err := manager.Status([]string{"stuck"})
	if err != nil {
		t.Fatal(err)
	}

	unsignalable(t, before[0].PID, func() {
		err := manager.Apply(nil)
		if err == nil || !strings.Contains(err.Error(), "stuck could not be stopped") {
			t.Fatalf("removal error = %v", err)
		}
		after, err := manager.Status([]string{"stuck"})
		if err != nil || after[0].PID != before[0].PID || !strings.Contains(after[0].LastError, "could not be stopped") {
			t.Fatalf("unstoppable process is no longer listed with its error: %+v, err = %v", after, err)
		}
		// The same removal is not retried, and it must not fail other changes.
		if err := manager.Apply(nil); err != nil {
			t.Fatalf("unchanged removal was retried: %v", err)
		}
		if err := manager.Apply([]config.Program{other}); err != nil {
			t.Fatalf("adding a program failed because of the unstoppable process: %v", err)
		}
		if statuses, err := manager.Status(nil); err != nil || len(statuses) != 2 {
			t.Fatalf("statuses = %+v, err = %v; want the unstoppable and the new program", statuses, err)
		}
	})

	// Once the process is gone on its own, the removal completes.
	if err := syscall.Kill(before[0].PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		statuses, _ := manager.Status([]string{"stuck"})
		return len(statuses) == 1 && statuses[0].PID == 0
	})
	if err := manager.Apply([]config.Program{other}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Status([]string{"stuck"}); err == nil {
		t.Fatal("exited process was not removed")
	}
}

func TestManagerRetriesFailedReplacementOnlyForANewDefinition(t *testing.T) {
	program := testProgram("worker", "sleep 30")
	other := testProgram("other", "sleep 30")
	manager := New([]config.Program{program})
	defer manager.StopAll()
	if err := manager.Start([]string{"worker"}); err != nil {
		t.Fatal(err)
	}
	before, err := manager.Status([]string{"worker"})
	if err != nil {
		t.Fatal(err)
	}
	edited := program
	edited.Args = []string{"-c", "sleep 29"}

	unsignalable(t, before[0].PID, func() {
		err := manager.Apply([]config.Program{edited})
		if err == nil || !strings.Contains(err.Error(), "worker could not be stopped") {
			t.Fatalf("replacement error = %v", err)
		}
		// The old run stays, and neither the same edit nor an unrelated change fails again.
		if err := manager.Apply([]config.Program{edited}); err != nil {
			t.Fatalf("unchanged edit was retried: %v", err)
		}
		if err := manager.Apply([]config.Program{edited, other}); err != nil {
			t.Fatalf("adding a program failed because of the unstoppable process: %v", err)
		}
		if statuses, err := manager.Status([]string{"worker"}); err != nil || statuses[0].PID != before[0].PID {
			t.Fatalf("old run was replaced: %+v, err = %v", statuses, err)
		}
		// Editing it again is a new request and is reported again.
		edited.Args = []string{"-c", "sleep 28"}
		if err := manager.Apply([]config.Program{edited, other}); err == nil {
			t.Fatal("a new edit of the unstoppable process was not reported")
		}
	})
}

func waitForReady(t *testing.T, ready string) {
	t.Helper()
	waitFor(t, time.Second, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})
}

// Status runs concurrently with the lifecycle actions, so everything it reads
// must be protected at a finer grain while Apply rewrites metadata in parallel.
func TestManagerStatusDoesNotRaceWithApply(t *testing.T) {
	first := testProgram("first", "sleep 30")
	first.Group = "workers"
	second := testProgram("second", "sleep 30")
	second.Group = "workers" // stays in the group while first moves in and out
	manager := New([]config.Program{first, second})
	defer manager.StopAll()

	var statusErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			if _, err := manager.Status([]string{"group:workers"}); err != nil {
				statusErr = err
				return
			}
			if _, err := manager.Status(nil); err != nil {
				statusErr = err
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		if i%2 == 0 {
			first.Group = "critical"
		} else {
			first.Group = "workers"
		}
		if err := manager.Apply([]config.Program{first, second}); err != nil {
			t.Fatal(err)
		}
	}
	<-done
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	if statuses, err := manager.Status([]string{"group:workers"}); err != nil || len(statuses) != 2 {
		t.Fatalf("group status = %+v, err = %v; want both programs", statuses, err)
	}
}

// Every kind of request hammers the same processes at once. Individual calls
// may fail ("already active" and the like); what must never happen is a data
// race, a deadlock, or a process left behind.
func TestManagerConcurrentLifecycleOperations(t *testing.T) {
	dir := t.TempDir()
	programs := []config.Program{
		testProgram("first", "sleep 30"), testProgram("second", "sleep 30"), testProgram("third", "sleep 30"),
	}
	for i := range programs {
		programs[i].StopTimeout = "100ms"
	}
	edited := append([]config.Program(nil), programs...)
	edited[0].Args = []string{"-c", "sleep 29"}
	renamed := []config.Program{programs[0], testProgram("renamed", "sleep 30"), programs[2]}
	states, err := NewProgramStateStore(filepath.Join(dir, ProgramStateFile))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewWithState(programs, nil, states)
	if err := manager.Start(nil); err != nil {
		t.Fatal(err)
	}

	var workers sync.WaitGroup
	hammer := func(request func(int)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < 60; i++ {
				request(i)
			}
		}()
	}
	hammer(func(int) { _, _ = manager.Status(nil); _, _ = manager.Status([]string{"group:default"}) })
	hammer(func(i int) {
		switch i % 3 {
		case 0:
			_ = manager.Apply(programs)
		case 1:
			_ = manager.Apply(edited)
		default:
			_ = manager.Apply(renamed)
		}
	})
	hammer(func(int) { _ = manager.Start([]string{"first"}); _ = manager.Stop([]string{"second"}) })
	hammer(func(int) {
		_ = manager.Restart([]string{"third"})
		_ = manager.RestartConfigured([]string{"first"}, programs)
	})
	hammer(func(int) { _ = manager.Pause([]string{"second"}); _ = manager.Resume([]string{"second"}) })
	workers.Wait()

	_ = manager.Apply(programs)
	if err := manager.StopAll(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second", "third", "renamed"} {
		if statuses, err := manager.Status([]string{name}); err == nil && statuses[0].PID != 0 {
			t.Fatalf("%s survived StopAll: %+v", name, statuses[0])
		}
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
