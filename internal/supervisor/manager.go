package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/CoolBanHub/pm/internal/config"
)

const (
	StateStopped  = "STOPPED"
	StateStarting = "STARTING"
	StateRunning  = "RUNNING"
	StateStopping = "STOPPING"
	StateExited   = "EXITED"
	StateBackoff  = "BACKOFF"
	StateFatal    = "FATAL"

	restartSnapshotBytes = 8 * 1024
)

type Status struct {
	Name        string    `json:"name"`
	Group       string    `json:"group"`
	Description string    `json:"description,omitempty"`
	State       string    `json:"state"`
	PID         int       `json:"pid,omitempty"`
	Uptime      string    `json:"uptime,omitempty"`
	Starts      int       `json:"starts"`
	Restarts    int       `json:"restarts"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	ExitedAt    time.Time `json:"exited_at,omitzero"`
	StdoutLog   string    `json:"stdout_log,omitempty"`
	StderrLog   string    `json:"stderr_log,omitempty"`
	Command     string    `json:"command"`
	Args        []string  `json:"args,omitempty"`
	Directory   string    `json:"directory,omitempty"`
	Autostart   bool      `json:"autostart"`
	Paused      bool      `json:"paused"`
	Disabled    bool      `json:"disabled"`
	Priority    int       `json:"priority"`
	Restart     string    `json:"restart_policy"`
	CPU         float64   `json:"cpu_percent"`
	Memory      int64     `json:"memory_bytes"`
	DiskUsage   int64     `json:"disk_bytes"`
	TCPPorts    []int     `json:"tcp_ports,omitempty"`
	Children    int       `json:"child_processes"`
	Descendants int       `json:"descendant_processes"`
	Goroutines  *int      `json:"goroutines,omitempty"`
	PprofURL    string    `json:"-"`
}

type Process struct {
	mu sync.Mutex
	// transition serializes the lifecycle changes to this one process (start,
	// stop, restart, replacement) so they never interleave. The Manager holds
	// it while waiting for the process to exit, and nothing else.
	transition sync.Mutex

	config  config.Program
	command *exec.Cmd
	state   string
	desired bool
	// stableRun records whether the current (or most recent) run survived the
	// start_secs window and therefore counts as a real RUNNING run rather
	// than a failed start.
	stableRun bool
	runID     uint64
	intentID  uint64
	// exited is closed once the process of the run has exited, and done once
	// its output has been written too, which a descendant can delay.
	exited      chan struct{}
	done        chan struct{}
	startedAt   time.Time
	exitedAt    time.Time
	exitCode    *int
	lastError   string
	starts      int
	restarts    int
	restartRuns []time.Time
	// stopFailed and pending record a change that could not be applied because
	// the process would not stop: pending is nil when that change was its
	// removal, or the definition it should have been replaced by.
	stopFailed bool
	pending    *config.Program
	events     *EventStore
}

// signalGroup delivers a signal to a managed process's process group. It is a
// variable so tests can simulate a process the daemon is not allowed to signal.
var signalGroup = syscall.Kill

func NewProcess(cfg config.Program) *Process {
	return newProcess(cfg, nil)
}

func newProcess(cfg config.Program, events *EventStore) *Process {
	return &Process{config: cfg, state: StateStopped, events: events}
}

func (p *Process) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.config.Disabled {
		return fmt.Errorf("%s is disabled", p.config.Name)
	}
	if p.config.Paused {
		return fmt.Errorf("%s is paused", p.config.Name)
	}
	if p.command != nil || p.state == StateBackoff {
		return fmt.Errorf("%s is already active", p.config.Name)
	}
	p.desired = true
	p.intentID++
	p.restartRuns = nil
	return p.startLocked(false)
}

func (p *Process) startLocked(restart bool) error {
	cmd := exec.Command(p.config.Command, p.config.Args...)
	cmd.Dir = p.config.Directory
	cmd.Env = mergeEnvironment(os.Environ(), p.config.Environment)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := openLog(p.config.StdoutLog, p.config.LogMaxBytes, p.config.LogBackups)
	if err != nil {
		return p.failStartLocked(fmt.Errorf("open stdout log: %w", err))
	}
	var stderr io.WriteCloser
	sharedLog := p.config.StderrLog != "" && p.config.StderrLog == p.config.StdoutLog
	if sharedLog {
		stderr = stdout
	} else {
		stderr, err = openLog(p.config.StderrLog, p.config.LogMaxBytes, p.config.LogBackups)
		if err != nil {
			closeLog(stdout)
			return p.failStartLocked(fmt.Errorf("open stderr log: %w", err))
		}
	}
	// The child writes to pipes rather than straight to the log files so that
	// the logs keep rotating. A run has nothing left to write once the process
	// and every descendant that inherited a pipe are gone.
	var streams []*output
	var writers []io.Closer
	attach := func(log io.WriteCloser, toStderr bool) error {
		stream, writer, err := newOutput(cmd, log, toStderr)
		if err != nil {
			return err
		}
		streams = append(streams, stream)
		writers = append(writers, writer)
		return nil
	}
	if stdout != nil {
		if err := attach(stdout, false); err != nil {
			closeLog(stdout)
			if !sharedLog {
				closeLog(stderr)
			}
			return p.failStartLocked(fmt.Errorf("open stdout log: %w", err))
		}
		if sharedLog {
			cmd.Stderr = cmd.Stdout
		}
	}
	if stderr != nil && !sharedLog {
		if err := attach(stderr, true); err != nil {
			for _, writer := range writers {
				writer.Close()
			}
			for _, stream := range streams {
				stream.abandon()
			}
			closeLog(stdout)
			closeLog(stderr)
			return p.failStartLocked(fmt.Errorf("open stderr log: %w", err))
		}
	}

	if err := cmd.Start(); err != nil {
		for _, writer := range writers {
			writer.Close()
		}
		// Closing the read ends ends the copies, which closes the log files.
		for _, stream := range streams {
			stream.abandon()
		}
		return p.failStartLocked(fmt.Errorf("start %s: %w", p.config.Name, err))
	}
	// The child has its own copies; until the parent's are closed the pipes
	// never report that every writer is gone.
	for _, writer := range writers {
		writer.Close()
	}

	p.runID++
	runID := p.runID
	intentID := p.intentID
	p.command = cmd
	p.exited = make(chan struct{})
	p.done = make(chan struct{})
	p.startedAt = time.Now()
	p.exitedAt = time.Time{}
	p.exitCode = nil
	p.lastError = ""
	p.starts++
	if restart {
		p.restarts++
	}
	if startWindow, _ := p.config.StartSecsDuration(); startWindow > 0 {
		p.state = StateStarting
		p.stableRun = false
		time.AfterFunc(startWindow, func() { p.markStable(runID) })
	} else {
		p.state = StateRunning
		p.stableRun = true
	}
	p.emitLocked("started", "")
	grace, err := p.config.StopTimeoutDuration()
	if err != nil {
		// Only a definition that bypassed validation gets here.
		grace, _ = config.DefaultProgram().StopTimeoutDuration()
	}
	go p.wait(cmd, runID, intentID, p.done, grace, streams)
	return nil
}

func (p *Process) failStartLocked(err error) error {
	p.command = nil
	p.desired = false
	p.state = StateFatal
	p.lastError = err.Error()
	p.exitedAt = time.Now()
	p.emitLocked("fatal", err.Error())
	return err
}

// markStable promotes a STARTING run to RUNNING once it has survived the
// configured start window. A newer run, an exited process, or a concurrent
// stop leaves the state untouched.
func (p *Process) markStable(runID uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runID == runID && p.command != nil && p.state == StateStarting {
		p.state = StateRunning
		p.stableRun = true
	}
}

// output is one of a run's output streams. The process writes to it, and so
// does every descendant that inherits it: drained is closed once the last of
// them has let go of it.
type output struct {
	reader  *os.File
	drained chan struct{}
	// copyErr is the error that ended the copy, if any. It is set before
	// drained is closed.
	copyErr error
}

// newOutput makes log the destination of the child's stdout or stderr: the
// child gets the write end of a pipe, and a goroutine copies the pipe into log
// until nothing holds that end. The caller closes its copy of the write end
// once the child has started.
func newOutput(cmd *exec.Cmd, log io.WriteCloser, toStderr bool) (*output, io.Closer, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	if toStderr {
		cmd.Stderr = writer
	} else {
		cmd.Stdout = writer
	}
	stream := &output{reader: reader, drained: make(chan struct{})}
	go func() {
		defer close(stream.drained)
		defer stream.forget()
		defer closeLog(log)
		defer reader.Close()
		_, stream.copyErr = io.Copy(log, reader)
	}()
	return stream, writer, nil
}

// abandon closes the read end, which ends the copy even while something still
// holds the write end.
func (o *output) abandon() { _ = o.reader.Close() }

// linger keeps copying a stream whose run is over. A detached descendant may
// still be writing, and closing the pipe would make it fail on its next
// write. Past lingeringLimit the oldest stream is let go instead.
func (o *output) linger() {
	lingering.Lock()
	defer lingering.Unlock()
	lingering.streams = append(lingering.streams, o)
	if len(lingering.streams) > lingeringLimit {
		lingering.streams[0].abandon()
		lingering.streams = lingering.streams[1:]
	}
}

// forget drops a stream from the lingering ones once its copy has ended.
func (o *output) forget() {
	lingering.Lock()
	defer lingering.Unlock()
	for i, stream := range lingering.streams {
		if stream == o {
			lingering.streams = append(lingering.streams[:i], lingering.streams[i+1:]...)
			return
		}
	}
}

// lingeringLimit is how many streams of ended runs may keep being copied, so
// that a program which keeps leaving writing descendants behind cannot exhaust
// the daemon's file descriptors.
const lingeringLimit = 16

var lingering struct {
	sync.Mutex
	streams []*output
}

// wait reports the end of a run once its process has exited and its output has
// been written. A descendant still writing after the process exited gets the
// time a stop may take to finish; past that the run is over and what is left
// of the process group is killed.
func (p *Process) wait(cmd *exec.Cmd, runID, intentID uint64, done chan struct{}, grace time.Duration, streams []*output) {
	err := cmd.Wait()
	close(p.exited)
	deadline := time.After(grace)
	var (
		cut     bool
		copyErr error
	)
	for _, stream := range streams {
		select {
		case <-stream.drained:
			copyErr = errors.Join(copyErr, stream.copyErr)
		case <-deadline:
			cut = true
		}
		if cut {
			break
		}
	}
	var notes []string
	if err != nil {
		notes = append(notes, err.Error())
	} else if copyErr != nil {
		notes = append(notes, copyErr.Error())
	}
	if cut {
		for _, stream := range streams {
			if !closed(stream.drained) {
				stream.linger()
			}
		}
		// A descendant in a session of its own is out of reach and keeps
		// writing to its stream; the rest of the process group is not.
		notes = append(notes, "background process kept the output open after the process exited")
		if leftover := alreadyGone(signalGroup(-cmd.Process.Pid, syscall.SIGKILL)); leftover != nil {
			notes = append(notes, leftover.Error())
		}
	}
	now := time.Now()
	code := cmd.ProcessState.ExitCode()

	p.mu.Lock()
	if p.command != cmd || p.runID != runID {
		p.mu.Unlock()
		return
	}
	failedStart := !p.stableRun
	p.command = nil
	p.exitedAt = now
	p.exitCode = &code
	p.state = StateExited
	p.lastError = strings.Join(notes, "; ")
	close(done)
	p.emitLocked("exited", p.lastError)

	shouldRestart := p.desired && p.intentID == intentID && restartWanted(p.config, code, failedStart)
	if !shouldRestart {
		p.desired = false
		p.mu.Unlock()
		return
	}

	window, _ := p.config.RestartWindowDuration()
	cutoff := now.Add(-window)
	kept := p.restartRuns[:0]
	for _, run := range p.restartRuns {
		if run.After(cutoff) {
			kept = append(kept, run)
		}
	}
	p.restartRuns = append(kept, now)
	if p.config.MaxRestarts > 0 && len(p.restartRuns) > p.config.MaxRestarts {
		p.desired = false
		p.state = StateFatal
		p.lastError = fmt.Sprintf("more than %d restarts within %s", p.config.MaxRestarts, p.config.RestartWindow)
		p.emitLocked("fatal", p.lastError)
		p.mu.Unlock()
		return
	}

	p.state = StateBackoff
	p.emitLocked("backoff", fmt.Sprintf("restarting in %s", p.config.RestartDelay))
	snapshot := RestartSnapshot{
		Trigger:     "automatic",
		Reason:      automaticRestartReason(p.config, code, failedStart),
		PreviousPID: cmd.Process.Pid,
		StartedAt:   p.startedAt,
		Uptime:      formatDuration(now.Sub(p.startedAt)),
		ExitCode:    intPointer(code),
		Error:       p.lastError,
	}
	delay, _ := p.config.RestartDelayDuration()
	p.mu.Unlock()

	timer := time.NewTimer(delay)
	defer timer.Stop()
	<-timer.C
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.desired || p.intentID != intentID || p.command != nil {
		return
	}
	p.emitRestartLocked(snapshot)
	_ = p.startLocked(true)
}

// restartWanted decides whether an exited process should be restarted. A run
// that never survived its start window is a failed start and retries under
// every policy except never, regardless of exit code (supervisor's startsecs
// semantics); afterwards the "unexpected" policy applies the configured
// expected exit codes.
func restartWanted(cfg config.Program, exitCode int, failedStart bool) bool {
	switch cfg.Restart {
	case "always":
		return true
	case "never":
		return false
	}
	if failedStart {
		return true
	}
	return !cfg.ExpectedExit(exitCode)
}

func automaticRestartReason(cfg config.Program, exitCode int, failedStart bool) string {
	if failedStart {
		return fmt.Sprintf("process exited with code %d during start window", exitCode)
	}
	if !cfg.ExpectedExit(exitCode) {
		return fmt.Sprintf("process exited with unexpected code %d", exitCode)
	}
	return fmt.Sprintf("process exited and restart policy is %s", cfg.Restart)
}

func intPointer(value int) *int {
	result := value
	return &result
}

func (p *Process) Stop() error {
	p.mu.Lock()
	p.desired = false
	p.intentID++
	if p.command == nil {
		previous := p.state
		p.state = StateStopped
		if previous != StateStopped {
			p.emitLocked("stopped", "")
		}
		p.mu.Unlock()
		return nil
	}
	cmd := p.command
	exited, done := p.exited, p.done
	p.state = StateStopping
	p.emitLocked("stopping", "")
	timeout, _ := p.config.StopTimeoutDuration()
	sig := stopSignal(p.config.StopSignal)
	p.mu.Unlock()

	err := signalGroup(-cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		// No such process group: the process has moved to another one.
		err = signalGroup(cmd.Process.Pid, sig)
	}
	if err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal %s: %w", p.config.Name, err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		// Kill the group, and the process itself unless it is known to have
		// exited: a process that left its group is in the second signal only.
		err := alreadyGone(signalGroup(-cmd.Process.Pid, syscall.SIGKILL))
		if !closed(exited) {
			err = errors.Join(err, alreadyGone(signalGroup(cmd.Process.Pid, syscall.SIGKILL)))
		}
		if err != nil {
			return fmt.Errorf("kill %s: %w", p.config.Name, err)
		}
		<-done
	}

	p.mu.Lock()
	if p.command == nil {
		p.state = StateStopped
		p.lastError = ""
		p.emitLocked("stopped", "")
	}
	p.mu.Unlock()
	return nil
}

// alreadyGone drops the error from signaling a process or group that has
// already exited.
func alreadyGone(err error) error {
	if err == nil || errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func closed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func (p *Process) emitLocked(eventType, message string) {
	if p.events == nil {
		return
	}
	event := Event{Program: p.config.Name, Type: eventType, State: p.state, Message: message, ExitCode: p.exitCode}
	if p.command != nil && p.command.Process != nil {
		event.PID = p.command.Process.Pid
	}
	p.events.Add(event)
}

func (p *Process) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := Status{
		Name:        p.config.Name,
		Group:       p.config.Group,
		Description: p.config.Description,
		State:       p.state,
		Starts:      p.starts,
		Restarts:    p.restarts,
		ExitCode:    p.exitCode,
		LastError:   p.lastError,
		StartedAt:   p.startedAt,
		ExitedAt:    p.exitedAt,
		StdoutLog:   p.config.StdoutLog,
		StderrLog:   p.config.StderrLog,
		Command:     p.config.Command,
		Args:        append([]string(nil), p.config.Args...),
		Directory:   p.config.Directory,
		Autostart:   p.config.Autostart,
		Paused:      p.config.Paused,
		Disabled:    p.config.Disabled,
		Priority:    p.config.Priority,
		Restart:     p.config.Restart,
		PprofURL:    p.config.PprofURL,
	}
	if p.command != nil && p.command.Process != nil {
		status.PID = p.command.Process.Pid
		status.Uptime = formatDuration(time.Since(p.startedAt))
	}
	return status
}

func (p *Process) ResetRestarts() error {
	p.mu.Lock()
	previous := p.restarts
	p.restarts = 0
	p.restartRuns = nil
	p.emitLocked("restarts_reset", fmt.Sprintf("restart count reset from %d to 0", previous))
	p.mu.Unlock()
	return nil
}

func (p *Process) emitRestart(snapshot RestartSnapshot) {
	p.mu.Lock()
	p.emitRestartLocked(snapshot)
	p.mu.Unlock()
}

func (p *Process) emitRestartLocked(snapshot RestartSnapshot) {
	snapshot.StdoutLog = p.config.StdoutLog
	snapshot.StderrLog = p.config.StderrLog
	snapshot.StdoutTail = readLogTail(snapshot.StdoutLog, restartSnapshotBytes)
	if snapshot.StderrLog != snapshot.StdoutLog {
		snapshot.StderrTail = readLogTail(snapshot.StderrLog, restartSnapshotBytes)
	}
	if p.events == nil {
		return
	}
	p.events.Add(Event{
		Program:  p.config.Name,
		Type:     "restart",
		State:    p.state,
		PID:      snapshot.PreviousPID,
		ExitCode: snapshot.ExitCode,
		Message:  snapshot.Reason,
		Restart:  &snapshot,
	})
}

func restartSnapshotFromStatus(status Status, trigger, reason string) RestartSnapshot {
	return RestartSnapshot{
		Trigger:     trigger,
		Reason:      reason,
		PreviousPID: status.PID,
		StartedAt:   status.StartedAt,
		Uptime:      status.Uptime,
		ExitCode:    status.ExitCode,
		Error:       status.LastError,
	}
}

func readLogTail(path string, limit int64) string {
	if path == "" || limit <= 0 {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	start := info.Size() - limit
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return ""
	}
	if start > 0 {
		if newline := strings.IndexByte(string(data), '\n'); newline >= 0 {
			data = data[newline+1:]
		}
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}

// Manager owns the managed processes and is safe for concurrent use. A short
// lock guards the process table and every process has its own transition lock
// for lifecycle changes, so waiting for one process to exit never delays
// requests for the others.
type Manager struct {
	mu        sync.RWMutex
	processes map[string]*Process
	// closed is set once StopAll begins. Later actions are refused so nothing
	// starts after the daemon has stopped its programs.
	closed bool

	events *EventStore
	states *ProgramStateStore
	// applyMu serializes reloads: each must finish replacing and stopping
	// before the next diffs the process table.
	applyMu sync.Mutex
	disk    atomic.Pointer[DiskMonitor]
	// retiring counts the shutdowns a reload started that are still in
	// flight; retiringIdle is closed whenever the count drops back to zero.
	retiringMu   sync.Mutex
	retiring     int
	retiringIdle chan struct{}
}

// processSet is a read-only view of the managed processes.
type processSet map[string]*Process

// shutdown is the old run of a process that must exit before a reload can
// finish: either the program was removed, or heir is the definition that
// replaces it.
type shutdown struct {
	name string
	old  *Process
	heir *config.Program
}

// refresh is a definition change that needs no restart, only new metadata and
// possibly a stop or a start.
type refresh struct {
	name    string
	program config.Program
}

// Apply reconciles the managed set with programs: removed processes are
// stopped, changed ones restarted, and new ones started. It returns once every
// affected process has settled. Shutdowns run at the same time and hold
// nothing but each process's own transition lock, so the whole reload waits at
// most one stop timeout and never delays requests for other processes.
func (m *Manager) Apply(programs []config.Program) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	programs = m.states.Apply(programs)
	next := make(map[string]config.Program, len(programs))
	programNames := make([]string, 0, len(programs))
	for _, program := range programs {
		next[program.Name] = program
		programNames = append(programNames, program.Name)
	}

	// Phase one only rewrites the process table, so it holds nothing while
	// waiting. New programs are listed at once but stay locked until the old
	// runs are gone, so a renamed program never races its old port.
	var (
		shutdowns []shutdown
		refreshes []refresh
		additions []*Process
	)
	m.mu.Lock()
	for name, process := range m.processes {
		program, configured := next[name]
		if pending, removal, blocked := process.pendingChange(); blocked {
			if process.alive() && removal == !configured && (removal || runtimeEqual(pending, program)) {
				// This very change already failed to stop the process: leave it alone.
				delete(next, name)
				continue
			}
			process.clearPendingChange()
		}
		delete(next, name)
		if !configured {
			delete(m.processes, name)
			shutdowns = append(shutdowns, shutdown{name: name, old: process})
			continue
		}
		current := process.program()
		switch {
		case reflect.DeepEqual(current, program):
		case runtimeEqual(current, program):
			refreshes = append(refreshes, refresh{name: name, program: program})
		default:
			heir := program
			shutdowns = append(shutdowns, shutdown{name: name, old: process, heir: &heir})
		}
	}
	for name, program := range next {
		process := newProcess(program, m.events)
		process.transition.Lock()
		m.processes[name] = process
		additions = append(additions, process)
	}
	for range shutdowns {
		m.trackRetiring()
	}
	m.mu.Unlock()

	errs := make([]error, len(shutdowns))
	var stopping sync.WaitGroup
	for i, item := range shutdowns {
		stopping.Add(1)
		go func() {
			defer stopping.Done()
			errs[i] = m.settle(item)
		}()
	}
	stopping.Wait()
	for _, process := range additions {
		var err error
		if shouldAutostart(process.program()) {
			err = m.launch(process)
		}
		process.transition.Unlock()
		errs = append(errs, err)
	}
	for _, item := range refreshes {
		program := m.states.Apply([]config.Program{item.program})[0]
		errs = append(errs, m.enter(item.name, false, func(p *Process) error { return p.refresh(program) }))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return m.states.Prune(programNames)
}

// settle ends the old run of a removed or superseded process. A superseding
// definition takes its place once the old run is gone, and starts if that run
// was active.
func (m *Manager) settle(item shutdown) error {
	defer m.untrackRetiring()
	item.old.transition.Lock()
	defer item.old.transition.Unlock()
	status := item.old.Status()
	if err := item.old.Stop(); err != nil {
		return m.stuck(item, err)
	}
	if item.heir == nil {
		return nil
	}
	heir := newProcess(*item.heir, m.events)
	heir.transition.Lock()
	defer heir.transition.Unlock()
	m.mu.Lock()
	if m.processes[item.name] == item.old {
		m.processes[item.name] = heir
	}
	m.mu.Unlock()
	if active(status.State) && runnable(*item.heir) {
		item.old.emitRestart(restartSnapshotFromStatus(status, "configuration", "runtime configuration changed"))
		return m.launch(heir)
	}
	return nil
}

// stuck records that item's process would not stop. It stays listed with the
// error, and later reloads retry only a different change, so one process the
// daemon cannot signal never fails unrelated ones.
func (m *Manager) stuck(item shutdown, cause error) error {
	err := fmt.Errorf("%s could not be stopped: %w", item.name, cause)
	item.old.block(item.heir, err.Error())
	m.mu.Lock()
	if _, listed := m.processes[item.name]; !listed {
		m.processes[item.name] = item.old
	}
	m.mu.Unlock()
	return err
}

func (m *Manager) trackRetiring() {
	m.retiringMu.Lock()
	m.retiring++
	m.retiringMu.Unlock()
}

func (m *Manager) untrackRetiring() {
	m.retiringMu.Lock()
	defer m.retiringMu.Unlock()
	if m.retiring--; m.retiring == 0 && m.retiringIdle != nil {
		close(m.retiringIdle)
		m.retiringIdle = nil
	}
}

// waitRetiring blocks until every shutdown a reload started has finished.
func (m *Manager) waitRetiring() {
	for {
		m.retiringMu.Lock()
		if m.retiring == 0 {
			m.retiringMu.Unlock()
			return
		}
		if m.retiringIdle == nil {
			m.retiringIdle = make(chan struct{})
		}
		idle := m.retiringIdle
		m.retiringMu.Unlock()
		<-idle
	}
}

var errClosed = errors.New("daemon is shutting down")

type unknownProgramError struct{ name string }

func (e *unknownProgramError) Error() string { return fmt.Sprintf("unknown program %q", e.name) }

// enter runs action on the process registered under name while holding that
// process's transition lock, so changes to one process never interleave and
// never make other processes wait. A reload may replace the process while the
// lock is held, in which case action runs on the replacement. Actions are
// refused once StopAll has begun unless stopping is set.
func (m *Manager) enter(name string, stopping bool, action func(*Process) error) error {
	for {
		m.mu.RLock()
		process, listed := m.processes[name]
		closed := m.closed
		m.mu.RUnlock()
		if !listed {
			return &unknownProgramError{name}
		}
		process.transition.Lock()
		m.mu.RLock()
		current := m.processes[name] == process
		closed = m.closed
		m.mu.RUnlock()
		if !current {
			process.transition.Unlock()
			continue
		}
		if closed && !stopping {
			process.transition.Unlock()
			return errClosed
		}
		err := action(process)
		process.transition.Unlock()
		return err
	}
}

// launch starts process unless the manager is shutting down. The caller holds
// the process's transition lock, which is what keeps StopAll from racing it.
func (m *Manager) launch(process *Process) error {
	m.mu.RLock()
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return errClosed
	}
	return process.Start()
}

func (p *Process) UpdateMetadata(program config.Program) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config.Group = program.Group
	p.config.Priority = program.Priority
	p.config.Autostart = program.Autostart
	p.config.Paused = program.Paused
	p.config.Disabled = program.Disabled
	p.config.PprofURL = program.PprofURL
	p.config.Source = program.Source
	p.emitLocked("configured", "metadata updated")
}

// group reads the group under the process lock. Status resolves group targets
// while other requests update metadata, so the read must not race them.
func (p *Process) group() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.config.Group
}

// program returns a copy of the process's definition under its lock.
func (p *Process) program() config.Program {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.config
}

// identity returns the process's name and priority under its lock.
func (p *Process) identity() (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.config.Name, p.config.Priority
}

// alive reports whether the process currently has a run.
func (p *Process) alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.command != nil
}

// restart stops the process and starts it again with program. The caller must
// hold the process's transition lock.
func (p *Process) restart(program config.Program) error {
	status := p.Status()
	if err := p.Stop(); err != nil {
		return err
	}
	p.emitRestart(restartSnapshotFromStatus(status, "manual", "restart requested"))
	p.mu.Lock()
	p.config = program
	p.mu.Unlock()
	return p.Start()
}

// refresh applies a definition change that needs no restart: new metadata,
// plus a stop or a start when the program just became, or stopped being,
// paused or disabled. The caller must hold the process's transition lock.
func (p *Process) refresh(program config.Program) error {
	p.mu.Lock()
	wasBlocked := p.config.Paused || p.config.Disabled
	p.mu.Unlock()
	p.UpdateMetadata(program)
	switch {
	case program.Paused || program.Disabled:
		return p.Stop()
	case wasBlocked && program.Autostart:
		return p.Start()
	}
	return nil
}

// pendingChange reports the change that could not be applied because the
// process would not stop: its removal, or its replacement by program. ok is
// false while no such change is pending.
func (p *Process) pendingChange() (program config.Program, removal, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stopFailed {
		return config.Program{}, false, false
	}
	if p.pending == nil {
		return config.Program{}, true, true
	}
	return *p.pending, false, true
}

func (p *Process) clearPendingChange() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopFailed = false
	p.pending = nil
}

// block records definition as the change that could not be applied because
// the process would not stop, and surfaces message on the process and in the
// event history.
func (p *Process) block(definition *config.Program, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopFailed = true
	p.pending = definition
	p.lastError = message
	p.emitLocked("fatal", message)
}

// active reports whether a process in state has a run that a reload carries
// over to the replacement.
func active(state string) bool {
	return state == StateStarting || state == StateRunning || state == StateBackoff || state == StateStopping
}

// runnable reports whether a program's paused or disabled markers allow it to run.
func runnable(program config.Program) bool {
	return !program.Paused && !program.Disabled
}

// runtimeEqual compares programs ignoring fields that never require a process
// restart: group, autostart, pause/disable markers, priority (ordering-only),
// pprof URL, and the defining file.
func runtimeEqual(left, right config.Program) bool {
	left.Group, right.Group = "", ""
	left.Autostart, right.Autostart = false, false
	left.Paused, right.Paused = false, false
	left.Disabled, right.Disabled = false, false
	left.Priority, right.Priority = 0, 0
	left.PprofURL, right.PprofURL = "", ""
	left.Source, right.Source = "", ""
	return reflect.DeepEqual(left, right)
}

func New(programs []config.Program) *Manager {
	events, _ := NewEventStore("", 1000)
	return NewWithState(programs, events, nil)
}

func NewWithEvents(programs []config.Program, events *EventStore) *Manager {
	return NewWithState(programs, events, nil)
}

func NewWithState(programs []config.Program, events *EventStore, states *ProgramStateStore) *Manager {
	programs = states.Apply(programs)
	m := &Manager{processes: make(map[string]*Process, len(programs)), events: events, states: states}
	for _, program := range programs {
		m.processes[program.Name] = newProcess(program, events)
	}
	return m
}

// SetDiskMonitor attaches a disk-usage monitor whose cached working-directory
// sizes are stamped onto each Status during collection. It is optional: when no
// monitor is set (e.g. in tests), disk fields are left at their zero value.
func (m *Manager) SetDiskMonitor(d *DiskMonitor) { m.disk.Store(d) }

// ApplyDiskUsage fills each status's DiskUsage from the monitor's in-memory
// cache. Safe to call on every status poll.
func (m *Manager) ApplyDiskUsage(statuses []Status) {
	if disk := m.disk.Load(); disk != nil {
		disk.Apply(statuses)
	}
}

// DiskTotal returns the total disk capacity backing the tracked working
// directories, or 0 when no monitor is attached.
func (m *Manager) DiskTotal() int64 {
	if disk := m.disk.Load(); disk != nil {
		return disk.DiskTotal()
	}
	return 0
}

func (m *Manager) Events(after uint64, limit int) []Event {
	return m.events.List(after, limit)
}

func (m *Manager) EventStore() *EventStore {
	return m.events
}

func (m *Manager) Autostart() []error {
	m.mu.RLock()
	set := processSet(m.processes)
	ordered := make([]*Process, 0, len(set))
	for _, name := range set.names() {
		ordered = append(ordered, set[name])
	}
	m.mu.RUnlock()
	sortByPriority(ordered, false)
	var errs []error
	for _, process := range ordered {
		if shouldAutostart(process.config) {
			if err := process.Start(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

// Start launches the selected processes in ascending priority so low-priority
// dependencies come up first.
func (m *Manager) Start(names []string) error {
	selected, err := m.selectProcesses(names)
	if err != nil {
		return err
	}
	sortByPriority(selected, false)
	return m.eachTransition(selected, (*Process).Start)
}

// Stop halts the selected processes in descending priority so dependents stop
// before the services they rely on.
func (m *Manager) Stop(names []string) error {
	selected, err := m.selectProcesses(names)
	if err != nil {
		return err
	}
	sortByPriority(selected, true)
	return m.eachTransition(selected, (*Process).Stop)
}

func (m *Manager) Restart(names []string) error {
	selected, err := m.selectProcesses(names)
	if err != nil {
		return err
	}
	sortByPriority(selected, false)
	return m.eachTransition(selected, func(p *Process) error { return p.restart(p.program()) })
}

// eachTransition applies action to each process in turn under that process's
// transition lock, so a slow one never holds up the rest.
func (m *Manager) eachTransition(selected []*Process, action func(*Process) error) error {
	var errs []error
	for _, process := range selected {
		name, _ := process.identity()
		errs = append(errs, m.enter(name, false, action))
	}
	return errors.Join(errs...)
}

func (m *Manager) ResetRestarts(names []string) error {
	return m.each(names, (*Process).ResetRestarts)
}

// RestartConfigured restarts the selected processes with their current
// definitions from the configuration file. All targets are validated before
// any running process is stopped.
func (m *Manager) RestartConfigured(names []string, programs []config.Program) error {
	programs = m.states.Apply(programs)
	configured := make(map[string]config.Program, len(programs))
	for _, program := range programs {
		configured[program.Name] = program
	}
	selected, err := m.selectProcesses(names)
	if err != nil {
		return err
	}
	type target struct {
		name    string
		process *Process
		program config.Program
	}
	targets := make([]target, 0, len(selected))
	for _, process := range selected {
		name, _ := process.identity()
		program, exists := configured[name]
		if !exists {
			return fmt.Errorf("program %q is no longer configured; run pm reload", name)
		}
		targets = append(targets, target{name: name, process: process, program: program})
	}

	var errs []error
	for _, item := range targets {
		errs = append(errs, m.enter(item.name, false, func(p *Process) error {
			program := item.program
			if p != item.process {
				// A reload replaced the process since validation; it already
				// carries the newest definition.
				program = p.program()
			}
			return p.restart(program)
		}))
	}
	return errors.Join(errs...)
}

func (m *Manager) Pause(names []string) error {
	return m.setMode(names, func(mode ProgramMode) ProgramMode {
		mode.Paused = true
		return mode
	}, false)
}

func (m *Manager) Resume(names []string) error {
	return m.setMode(names, func(mode ProgramMode) ProgramMode {
		mode.Paused = false
		return mode
	}, true)
}

func (m *Manager) Disable(names []string) error {
	return m.setMode(names, func(mode ProgramMode) ProgramMode {
		mode.Disabled = true
		return mode
	}, false)
}

func (m *Manager) Enable(names []string) error {
	return m.setMode(names, func(mode ProgramMode) ProgramMode {
		mode.Disabled = false
		return mode
	}, true)
}

// setMode persists a paused or disabled change for the selected programs and
// applies it to each process. The processes take their mode from the persistent
// state rather than from this call, so concurrent changes to the same program
// always leave it matching what was persisted last.
func (m *Manager) setMode(names []string, update func(ProgramMode) ProgramMode, start bool) error {
	selected, err := m.selectProcesses(names)
	if err != nil {
		return err
	}
	targets := make([]string, 0, len(selected))
	for _, process := range selected {
		name, _ := process.identity()
		targets = append(targets, name)
	}
	if err := m.states.Set(targets, update); err != nil {
		return err
	}
	var errs []error
	for _, name := range targets {
		errs = append(errs, m.enter(name, false, func(p *Process) error {
			mode := m.states.Apply([]config.Program{{Name: name}})[0]
			p.mu.Lock()
			p.config.Paused, p.config.Disabled = mode.Paused, mode.Disabled
			p.mu.Unlock()
			switch {
			case mode.Paused || mode.Disabled:
				return p.Stop()
			case start:
				return p.Start()
			}
			return nil
		}))
	}
	return errors.Join(errs...)
}

// StopAll stops every managed program in priority order and refuses further
// actions. It waits for the shutdowns a reload still has in flight, so nothing
// outlives the daemon.
func (m *Manager) StopAll() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()

	selected, err := m.selectProcesses([]string{"all"})
	if err != nil {
		return err
	}
	sortByPriority(selected, true)
	var errs []error
	for _, process := range selected {
		// A reload may take a process out from under StopAll; its own shutdown
		// covers it, so only other failures are reported.
		name, _ := process.identity()
		if err := m.enter(name, true, (*Process).Stop); err != nil {
			var unknown *unknownProgramError
			if !errors.As(err, &unknown) {
				errs = append(errs, err)
			}
		}
	}
	m.waitRetiring()
	return errors.Join(errs...)
}

// Status reports the selected processes. It takes no lock that a stop or
// restart holds while waiting, so polling never falls behind one.
func (m *Manager) Status(names []string) ([]Status, error) {
	selected, err := m.selectProcesses(names)
	if err != nil {
		return nil, err
	}
	statuses := make([]Status, 0, len(selected))
	for _, process := range selected {
		statuses = append(statuses, process.Status())
	}
	return statuses, nil
}

func (m *Manager) each(names []string, action func(*Process) error) error {
	selected, err := m.selectProcesses(names)
	if err != nil {
		return err
	}
	return runEach(selected, action)
}

func runEach(selected []*Process, action func(*Process) error) error {
	var errs []error
	for _, process := range selected {
		if err := action(process); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// groupTarget recognizes the two accepted group selectors: "group:web" and
// supervisor-style "web:*". Both address every program in group web.
func groupTarget(name string) (string, bool) {
	if group, ok := strings.CutPrefix(name, "group:"); ok && group != "" {
		return group, true
	}
	if group, ok := strings.CutSuffix(name, ":*"); ok && group != "" {
		return group, true
	}
	return "", false
}

// selectProcesses resolves programs, groups, and "all" against the process table.
func (m *Manager) selectProcesses(names []string) ([]*Process, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return selectProcesses(processSet(m.processes), names)
}

func selectProcesses(set processSet, names []string) ([]*Process, error) {
	if len(names) == 0 || (len(names) == 1 && names[0] == "all") {
		names = set.names()
	}
	selected := make([]*Process, 0, len(names))
	seen := make(map[*Process]struct{}, len(names))
	for _, name := range names {
		if group, isGroup := groupTarget(name); isGroup {
			matched := false
			for _, processName := range set.names() {
				process := set[processName]
				if process.group() != group {
					continue
				}
				matched = true
				if _, duplicate := seen[process]; !duplicate {
					seen[process] = struct{}{}
					selected = append(selected, process)
				}
			}
			if !matched {
				return nil, fmt.Errorf("unknown group %q", group)
			}
			continue
		}
		process, ok := set[name]
		if !ok {
			return nil, fmt.Errorf("unknown program %q", name)
		}
		if _, duplicate := seen[process]; !duplicate {
			seen[process] = struct{}{}
			selected = append(selected, process)
		}
	}
	return selected, nil
}

// sortByPriority orders processes for lifecycle operations: startup runs in
// ascending priority (dependencies first), shutdown in descending priority so
// dependents stop first. Name order breaks ties deterministically.
func sortByPriority(list []*Process, shutdown bool) {
	type entry struct {
		process *Process
		name    string
		prio    int
	}
	entries := make([]entry, len(list))
	for i, process := range list {
		entries[i].process = process
		entries[i].name, entries[i].prio = process.identity()
	}
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := entries[i], entries[j]
		if left.prio != right.prio {
			if shutdown {
				return left.prio > right.prio
			}
			return left.prio < right.prio
		}
		return left.name < right.name
	})
	for i := range entries {
		list[i] = entries[i].process
	}
}

// names lists the set's processes in sorted order.
func (set processSet) names() []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func shouldAutostart(program config.Program) bool {
	return program.Autostart && !program.Paused && !program.Disabled
}

func mergeEnvironment(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	values := make(map[string]string, len(base)+len(extra))
	for _, item := range base {
		key, value, found := strings.Cut(item, "=")
		if found {
			values[key] = value
		}
	}
	for key, value := range extra {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func closeLog(closer io.Closer) {
	if closer != nil {
		_ = closer.Close()
	}
}

func stopSignal(name string) syscall.Signal {
	switch strings.ToUpper(name) {
	case "INT":
		return syscall.SIGINT
	case "QUIT":
		return syscall.SIGQUIT
	case "HUP":
		return syscall.SIGHUP
	case "USR1":
		return syscall.SIGUSR1
	case "USR2":
		return syscall.SIGUSR2
	default:
		return syscall.SIGTERM
	}
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Second {
		return "0s"
	}
	return d.String()
}
