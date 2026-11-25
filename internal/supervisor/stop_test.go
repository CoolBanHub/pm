package supervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/CoolBanHub/pm/internal/config"
)

// helperLeftoverChild is the entry point of the helper processes that
// leftoverChildProgram runs as a managed program. "spawner" and "exiter" start
// a copy of the test binary and then report readiness; "spawner" waits to be
// signaled while "exiter" exits at once. The copy is one of:
//
//   - "detached": in a session of its own, so no signal to the spawner's
//     process group reaches it, still holding the run's output pipes;
//   - "writer": detached as well, and writing a line to stdout every 20ms;
//   - "attached": in the spawner's group and ignoring SIGTERM, like a stuck
//     worker;
//   - "quiet": in the spawner's group with its output closed, like a helper
//     that redirects what it writes.
//
// Each reports its pid and then never exits.
func helperLeftoverChild(t *testing.T) {
	t.Helper()
	dir := os.Getenv("PM_TEST_DIR")
	switch mode := os.Getenv("PM_TEST_HELPER"); mode {
	case "":
		t.Skip("runs only as a helper process")
	case "spawner", "exiter":
		kind := os.Getenv("PM_TEST_CHILD")
		child := exec.Command(os.Args[0], "-test.run=^TestHelperLeftoverChild$")
		child.Env = append(os.Environ(), "PM_TEST_HELPER="+kind)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if kind == "detached" || kind == "writer" {
			child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dir, "child")
		waitFor(t, 5*time.Second, func() bool {
			_, err := os.Stat(marker)
			return err == nil
		})
		if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("ready"), 0o644); err != nil {
			t.Fatal(err)
		}
		if mode == "exiter" {
			os.Exit(0)
		}
		select {}
	case "attached":
		signal.Ignore(syscall.SIGTERM)
	case "quiet":
		// The pipe write ends this process was started with.
		_ = syscall.Close(1)
		_ = syscall.Close(2)
	case "writer":
		go func() {
			for {
				fmt.Fprintln(os.Stdout, "line")
				time.Sleep(20 * time.Millisecond)
			}
		}()
	case "detached":
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		os.Exit(3)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestHelperLeftoverChild(t *testing.T) { helperLeftoverChild(t) }

// leftoverChildProgram is a managed program that leaves a child of the given
// kind behind. It returns the program, the file the spawner writes once the
// child is up, and the file the child writes its pid to.
func leftoverChildProgram(t *testing.T, mode, kind, stopTimeout string) (config.Program, string, string) {
	t.Helper()
	dir := t.TempDir()
	command, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	program := testProgram("spawner", "")
	program.Command = command
	program.Args = []string{"-test.run=^TestHelperLeftoverChild$"}
	program.Environment = map[string]string{"PM_TEST_HELPER": mode, "PM_TEST_CHILD": kind, "PM_TEST_DIR": dir}
	program.StdoutLog = filepath.Join(dir, "stdout.log")
	program.StderrLog = filepath.Join(dir, "stderr.log")
	program.StopTimeout = stopTimeout
	return program, filepath.Join(dir, "ready"), filepath.Join(dir, "child")
}

// childPID is the pid the helper's child reported, which it does before the
// program is ready.
func childPID(t *testing.T, marker string) int {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("helper child never started: %v", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatalf("helper child pid %q: %v", data, err)
	}
	return pid
}

// endChildWithTest makes sure the helper's child, which never exits on its
// own, does not outlive the test.
func endChildWithTest(t *testing.T, marker string) {
	t.Helper()
	t.Cleanup(func() {
		if data, err := os.ReadFile(marker); err == nil {
			if pid, _ := strconv.Atoi(string(data)); pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}

func stopWithin(t *testing.T, process *Process, limit time.Duration) {
	t.Helper()
	stopped := make(chan error, 1)
	go func() { stopped <- process.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(limit):
		t.Fatal("Stop did not return")
	}
}

func TestStopReturnsWhileADetachedChildHoldsTheOutput(t *testing.T) {
	program, ready, marker := leftoverChildProgram(t, "spawner", "detached", "300ms")
	process := newProcess(program, nil)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	endChildWithTest(t, marker)
	waitForReady(t, ready)

	stopWithin(t, process, 3*time.Second)
	if status := process.Status(); status.State != StateStopped || status.PID != 0 || status.LastError != "" {
		t.Fatalf("unexpected status after Stop: %+v", status)
	}
	if err := syscall.Kill(childPID(t, marker), 0); err != nil {
		t.Fatalf("detached child was affected by the stop: %v", err)
	}
}

func TestExitIsObservedWhileADetachedChildHoldsTheOutput(t *testing.T) {
	program, ready, marker := leftoverChildProgram(t, "exiter", "detached", "300ms")
	process := newProcess(program, nil)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	endChildWithTest(t, marker)
	waitForReady(t, ready)

	waitFor(t, 3*time.Second, func() bool { return process.Status().State == StateExited })
	status := process.Status()
	if status.PID != 0 || !strings.Contains(status.LastError, "background process kept the output open") {
		t.Fatalf("unexpected status after the process exited: %+v", status)
	}
	if err := syscall.Kill(childPID(t, marker), 0); err != nil {
		t.Fatalf("detached child was ended: %v", err)
	}
}

func TestChildLeftInTheGroupIsEndedOnceTheOutputTimesOut(t *testing.T) {
	program, ready, marker := leftoverChildProgram(t, "exiter", "attached", "300ms")
	process := newProcess(program, nil)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	endChildWithTest(t, marker)
	waitForReady(t, ready)
	child := childPID(t, marker)

	stopWithin(t, process, 3*time.Second)
	if status := process.Status(); status.State != StateStopped {
		t.Fatalf("unexpected status after Stop: %+v", status)
	}
	// The child ignores SIGTERM, so only the kill after the timeout ends it.
	waitFor(t, time.Second, func() bool {
		return errors.Is(syscall.Kill(child, 0), syscall.ESRCH)
	})
}

// A child that lets go of the output is neither waited for nor ended, as
// before: only what still writes keeps a run open.
func TestChildThatLetGoOfTheOutputIsLeftAlone(t *testing.T) {
	program, ready, marker := leftoverChildProgram(t, "exiter", "quiet", "10s")
	process := newProcess(program, nil)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	endChildWithTest(t, marker)
	waitForReady(t, ready)

	waitFor(t, 2*time.Second, func() bool { return process.Status().State == StateExited })
	if status := process.Status(); status.PID != 0 || status.LastError != "" {
		t.Fatalf("unexpected status after the process exited: %+v", status)
	}
	if err := syscall.Kill(childPID(t, marker), 0); err != nil {
		t.Fatalf("child that let go of the output was ended: %v", err)
	}
}

func TestOutputStreamsShareALogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.log")
	program := testProgram("writer", "echo to-stdout; echo to-stderr >&2")
	program.StdoutLog = path
	program.StderrLog = path
	process := newProcess(program, nil)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return process.Status().State == StateExited })

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if output := string(data); !strings.Contains(output, "to-stdout") || !strings.Contains(output, "to-stderr") {
		t.Fatalf("shared log = %q", output)
	}
}

// Each run copies its output with its own goroutines. They are gone once the
// run is over, or, for a detached child that is still writing, as soon as that
// child is.
func TestOutputCopiesEndWithTheirRuns(t *testing.T) {
	baseline := runtime.NumGoroutine()
	for _, kind := range []string{"ended", "detached"} {
		var (
			program  config.Program
			marker   string
			children []int
		)
		t.Cleanup(func() {
			for _, child := range children {
				_ = syscall.Kill(child, syscall.SIGKILL)
			}
		})
		if kind == "ended" {
			program = testProgram("writer", "echo line")
			program.StdoutLog = filepath.Join(t.TempDir(), "out.log")
			program.StderrLog = filepath.Join(t.TempDir(), "err.log")
		} else {
			program, _, marker = leftoverChildProgram(t, "exiter", kind, "200ms")
		}
		process := newProcess(program, nil)
		for range 5 {
			if err := process.Start(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 3*time.Second, func() bool { return process.Status().State == StateExited })
			// The child reports before the program exits, so this is its run's.
			if marker != "" {
				children = append(children, childPID(t, marker))
			}
		}
		if marker == "" {
			waitFor(t, 3*time.Second, func() bool { return runtime.NumGoroutine() <= baseline })
			continue
		}
		// One child per run outlives its run, still holding its output.
		for _, child := range children {
			if err := syscall.Kill(child, 0); err != nil {
				t.Fatalf("detached child %d was ended: %v", child, err)
			}
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
		waitFor(t, 3*time.Second, func() bool { return runtime.NumGoroutine() <= baseline })
	}
}

// fileSize is the size of path, or 0 when it cannot be read.
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// A detached process that keeps writing must outlive its run: closing the
// output would make its next write the end of it.
func TestDetachedWriterOutlivesItsRunAndKeepsLogging(t *testing.T) {
	program, ready, marker := leftoverChildProgram(t, "exiter", "writer", "200ms")
	process := newProcess(program, nil)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	endChildWithTest(t, marker)
	waitForReady(t, ready)
	waitFor(t, 3*time.Second, func() bool { return process.Status().State == StateExited })

	written := fileSize(program.StdoutLog)
	waitFor(t, 3*time.Second, func() bool { return fileSize(program.StdoutLog) > written+100 })
	if err := syscall.Kill(childPID(t, marker), 0); err != nil {
		t.Fatalf("detached writer did not survive the end of its run: %v", err)
	}
}

func TestLingeringStreamsAreCapped(t *testing.T) {
	var streams []*output
	t.Cleanup(func() {
		lingering.Lock()
		lingering.streams = nil
		lingering.Unlock()
		for _, stream := range streams {
			stream.abandon()
		}
	})
	lingering.Lock()
	lingering.streams = nil
	lingering.Unlock()

	for range lingeringLimit + 2 {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		stream := &output{reader: reader}
		stream.linger()
		streams = append(streams, stream)
	}
	// The two oldest were let go of; the rest are still open.
	for i, stream := range streams {
		if _, err := stream.reader.Stat(); (i < 2) != errors.Is(err, os.ErrClosed) {
			t.Fatalf("stream %d of %d: stat error = %v", i, len(streams), err)
		}
	}
	lingering.Lock()
	count := len(lingering.streams)
	lingering.Unlock()
	if count != lingeringLimit {
		t.Fatalf("lingering streams = %d, want %d", count, lingeringLimit)
	}
	for _, stream := range streams[2:] {
		stream.forget()
	}
	lingering.Lock()
	count = len(lingering.streams)
	lingering.Unlock()
	if count != 0 {
		t.Fatalf("lingering streams after the copies ended = %d, want 0", count)
	}
}

func TestStopSignalsTheProcessItselfWhenItLeftItsGroup(t *testing.T) {
	program, ready := stubbornProgram(t)
	process := NewProcess(program)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	waitForReady(t, ready)
	pid := process.Status().PID

	// The process ignores SIGTERM, and signals to its process group go nowhere,
	// as they would for a process that moved to another group.
	deliver := signalGroup
	var direct []syscall.Signal
	signalGroup = func(target int, sig syscall.Signal) error {
		if target == -pid {
			return syscall.ESRCH
		}
		if target == pid {
			direct = append(direct, sig)
		}
		return deliver(target, sig)
	}
	defer func() { signalGroup = deliver }()

	stopWithin(t, process, 3*time.Second)
	if len(direct) != 2 || direct[0] != syscall.SIGTERM || direct[1] != syscall.SIGKILL {
		t.Fatalf("signals sent to the process itself = %v; want TERM, then KILL", direct)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process survived Stop: %v", err)
	}
	if status := process.Status(); status.State != StateStopped || status.LastError != "" {
		t.Fatalf("unexpected status after Stop: %+v", status)
	}
}
