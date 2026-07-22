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
	"syscall"
	"time"

	"github.com/CoolBanHub/pm/internal/config"
)

const (
	StateStopped  = "STOPPED"
	StateRunning  = "RUNNING"
	StateStopping = "STOPPING"
	StateExited   = "EXITED"
	StateBackoff  = "BACKOFF"
	StateFatal    = "FATAL"
)

type Status struct {
	Name        string    `json:"name"`
	Group       string    `json:"group"`
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
	Restart     string    `json:"restart_policy"`
	CPU         float64   `json:"cpu_percent"`
	Memory      int64     `json:"memory_bytes"`
	Children    int       `json:"child_processes"`
	Descendants int       `json:"descendant_processes"`
	Goroutines  *int      `json:"goroutines,omitempty"`
	PprofURL    string    `json:"-"`
}

type Process struct {
	mu sync.Mutex

	config      config.Program
	command     *exec.Cmd
	state       string
	desired     bool
	runID       uint64
	intentID    uint64
	done        chan struct{}
	startedAt   time.Time
	exitedAt    time.Time
	exitCode    *int
	lastError   string
	starts      int
	restarts    int
	restartRuns []time.Time
	events      *EventStore
}

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
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if stderr != nil {
		cmd.Stderr = stderr
	}

	if err := cmd.Start(); err != nil {
		closeLog(stdout)
		if !sharedLog {
			closeLog(stderr)
		}
		return p.failStartLocked(fmt.Errorf("start %s: %w", p.config.Name, err))
	}

	p.runID++
	runID := p.runID
	intentID := p.intentID
	p.command = cmd
	p.done = make(chan struct{})
	p.state = StateRunning
	p.startedAt = time.Now()
	p.exitedAt = time.Time{}
	p.exitCode = nil
	p.lastError = ""
	p.starts++
	if restart {
		p.restarts++
	}
	p.emitLocked("started", "")
	logs := []io.Closer{stdout}
	if !sharedLog {
		logs = append(logs, stderr)
	}
	go p.wait(cmd, runID, intentID, p.done, logs...)
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

func (p *Process) wait(cmd *exec.Cmd, runID, intentID uint64, done chan struct{}, logs ...io.Closer) {
	err := cmd.Wait()
	for _, writer := range logs {
		closeLog(writer)
	}
	now := time.Now()
	code := cmd.ProcessState.ExitCode()

	p.mu.Lock()
	if p.command != cmd || p.runID != runID {
		p.mu.Unlock()
		return
	}
	p.command = nil
	p.exitedAt = now
	p.exitCode = &code
	p.state = StateExited
	if err != nil {
		p.lastError = err.Error()
	} else {
		p.lastError = ""
	}
	close(done)
	p.emitLocked("exited", p.lastError)

	shouldRestart := p.desired && p.intentID == intentID && (p.config.Restart == "always" || (p.config.Restart == "unexpected" && code != 0))
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
	_ = p.startLocked(true)
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
	done := p.done
	p.state = StateStopping
	p.emitLocked("stopping", "")
	timeout, _ := p.config.StopTimeoutDuration()
	sig := stopSignal(p.config.StopSignal)
	p.mu.Unlock()

	err := syscall.Kill(-cmd.Process.Pid, sig)
	if err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal %s: %w", p.config.Name, err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
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
		Name:      p.config.Name,
		Group:     p.config.Group,
		State:     p.state,
		Starts:    p.starts,
		Restarts:  p.restarts,
		ExitCode:  p.exitCode,
		LastError: p.lastError,
		StartedAt: p.startedAt,
		ExitedAt:  p.exitedAt,
		StdoutLog: p.config.StdoutLog,
		StderrLog: p.config.StderrLog,
		Command:   p.config.Command,
		Args:      append([]string(nil), p.config.Args...),
		Directory: p.config.Directory,
		Autostart: p.config.Autostart,
		Paused:    p.config.Paused,
		Disabled:  p.config.Disabled,
		Restart:   p.config.Restart,
		PprofURL:  p.config.PprofURL,
	}
	if p.command != nil && p.command.Process != nil {
		status.PID = p.command.Process.Pid
		status.Uptime = formatDuration(time.Since(p.startedAt))
	}
	return status
}

type Manager struct {
	processes map[string]*Process
	events    *EventStore
	states    *ProgramStateStore
	modeMu    sync.Mutex
}

func (m *Manager) Apply(programs []config.Program) error {
	programs = m.states.Apply(programs)
	programNames := make([]string, 0, len(programs))
	next := make(map[string]config.Program, len(programs))
	for _, program := range programs {
		next[program.Name] = program
		programNames = append(programNames, program.Name)
	}
	var errs []error
	for name, process := range m.processes {
		program, exists := next[name]
		if !exists {
			if err := process.Stop(); err != nil {
				errs = append(errs, err)
				continue
			}
			delete(m.processes, name)
			continue
		}
		delete(next, name)
		if reflect.DeepEqual(process.config, program) {
			continue
		}
		if runtimeEqual(process.config, program) {
			wasBlocked := process.config.Paused || process.config.Disabled
			process.UpdateMetadata(program)
			isBlocked := program.Paused || program.Disabled
			if isBlocked {
				if err := process.Stop(); err != nil {
					errs = append(errs, err)
				}
			} else if wasBlocked && program.Autostart {
				if err := process.Start(); err != nil {
					errs = append(errs, err)
				}
			}
			continue
		}
		status := process.Status()
		wasActive := status.State == StateRunning || status.State == StateBackoff || status.State == StateStopping
		if err := process.Stop(); err != nil {
			errs = append(errs, err)
			continue
		}
		replacement := newProcess(program, m.events)
		m.processes[name] = replacement
		if wasActive && !program.Paused && !program.Disabled {
			if err := replacement.Start(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for name, program := range next {
		process := newProcess(program, m.events)
		m.processes[name] = process
		if shouldAutostart(program) {
			if err := process.Start(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return m.states.Prune(programNames)
}

func (p *Process) UpdateMetadata(program config.Program) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.config.Group = program.Group
	p.config.Autostart = program.Autostart
	p.config.Paused = program.Paused
	p.config.Disabled = program.Disabled
	p.config.PprofURL = program.PprofURL
	p.emitLocked("configured", "metadata updated")
}

func runtimeEqual(left, right config.Program) bool {
	left.Group, right.Group = "", ""
	left.Autostart, right.Autostart = false, false
	left.Paused, right.Paused = false, false
	left.Disabled, right.Disabled = false, false
	left.PprofURL, right.PprofURL = "", ""
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

func (m *Manager) Events(after uint64, limit int) []Event {
	return m.events.List(after, limit)
}

func (m *Manager) EventStore() *EventStore {
	return m.events
}

func (m *Manager) Autostart() []error {
	var errs []error
	for _, name := range m.names() {
		process := m.processes[name]
		if shouldAutostart(process.config) {
			if err := process.Start(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

func (m *Manager) Start(names []string) error {
	return m.each(names, (*Process).Start)
}

func (m *Manager) Stop(names []string) error {
	return m.each(names, (*Process).Stop)
}

func (m *Manager) Restart(names []string) error {
	return m.each(names, func(p *Process) error {
		if err := p.Stop(); err != nil {
			return err
		}
		return p.Start()
	})
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
		process *Process
		program config.Program
	}
	targets := make([]target, 0, len(selected))
	for _, process := range selected {
		name := process.Status().Name
		program, exists := configured[name]
		if !exists {
			return fmt.Errorf("program %q is no longer configured; run pm reload", name)
		}
		targets = append(targets, target{process: process, program: program})
	}

	var errs []error
	for _, item := range targets {
		if err := item.process.Stop(); err != nil {
			errs = append(errs, err)
			continue
		}
		item.process.mu.Lock()
		item.process.config = item.program
		item.process.mu.Unlock()
		if err := item.process.Start(); err != nil {
			errs = append(errs, err)
		}
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

func (m *Manager) setMode(names []string, update func(ProgramMode) ProgramMode, start bool) error {
	m.modeMu.Lock()
	defer m.modeMu.Unlock()
	selected, err := m.selectProcesses(names)
	if err != nil {
		return err
	}
	resolvedNames := make([]string, 0, len(selected))
	for _, process := range selected {
		resolvedNames = append(resolvedNames, process.Status().Name)
	}
	if err := m.states.Set(resolvedNames, update); err != nil {
		return err
	}
	var errs []error
	for _, process := range selected {
		process.mu.Lock()
		mode := update(ProgramMode{Paused: process.config.Paused, Disabled: process.config.Disabled})
		process.config.Paused = mode.Paused
		process.config.Disabled = mode.Disabled
		blocked := mode.Paused || mode.Disabled
		process.mu.Unlock()
		if blocked || !start {
			if err := process.Stop(); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err := process.Start(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) StopAll() error {
	return m.Stop([]string{"all"})
}

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
	var errs []error
	for _, process := range selected {
		if err := action(process); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) selectProcesses(names []string) ([]*Process, error) {
	if len(names) == 0 || (len(names) == 1 && names[0] == "all") {
		names = m.names()
	}
	selected := make([]*Process, 0, len(names))
	for _, name := range names {
		process, ok := m.processes[name]
		if !ok {
			return nil, fmt.Errorf("unknown program %q", name)
		}
		selected = append(selected, process)
	}
	return selected, nil
}

func (m *Manager) names() []string {
	names := make([]string, 0, len(m.processes))
	for name := range m.processes {
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
