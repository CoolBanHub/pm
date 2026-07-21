package supervisor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Event struct {
	ID       uint64    `json:"id"`
	Time     time.Time `json:"time"`
	Program  string    `json:"program,omitempty"`
	Type     string    `json:"type"`
	State    string    `json:"state,omitempty"`
	PID      int       `json:"pid,omitempty"`
	ExitCode *int      `json:"exit_code,omitempty"`
	Message  string    `json:"message,omitempty"`
}

type EventStore struct {
	mu       sync.RWMutex
	capacity int
	nextID   uint64
	events   []Event
	file     *os.File
	path     string
	writes   int
}

func NewEventStore(path string, capacity int) (*EventStore, error) {
	store := &EventStore{capacity: capacity, nextID: 1, path: path}
	if capacity == 0 || path == "" {
		return store, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create event directory: %w", err)
	}
	if err := store.load(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open event history: %w", err)
	}
	store.file = file
	return store, nil
}

func (s *EventStore) load(path string) error {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read event history: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		s.events = append(s.events, event)
		if event.ID >= s.nextID {
			s.nextID = event.ID + 1
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan event history: %w", err)
	}
	s.trimLocked()
	return nil
}

func (s *EventStore) Add(event Event) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capacity == 0 {
		return event
	}
	event.ID = s.nextID
	s.nextID++
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	s.events = append(s.events, event)
	s.trimLocked()
	if s.file != nil {
		if data, err := json.Marshal(event); err == nil {
			_, _ = s.file.Write(append(data, '\n'))
			s.writes++
			if s.writes >= s.capacity {
				_ = s.compactLocked()
			}
		}
	}
	return event
}

func (s *EventStore) compactLocked() error {
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".events-*")
	if err != nil {
		return err
	}
	tempPath := temporary.Name()
	defer os.Remove(tempPath)
	encoder := json.NewEncoder(temporary)
	for _, event := range s.events {
		if err := encoder.Encode(event); err != nil {
			temporary.Close()
			return err
		}
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := s.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		s.file, _ = os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		return err
	}
	s.file, err = os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	s.writes = 0
	return err
}

func (s *EventStore) List(after uint64, limit int) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > s.capacity {
		limit = s.capacity
	}
	result := make([]Event, 0, limit)
	for i := len(s.events) - 1; i >= 0 && len(result) < limit; i-- {
		if s.events[i].ID > after {
			result = append(result, s.events[i])
		}
	}
	return result
}

func (s *EventStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

func (s *EventStore) trimLocked() {
	if s.capacity > 0 && len(s.events) > s.capacity {
		s.events = append([]Event(nil), s.events[len(s.events)-s.capacity:]...)
	}
}
