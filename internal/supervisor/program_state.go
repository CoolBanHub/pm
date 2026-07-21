package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/CoolBanHub/pm/internal/config"
)

const ProgramStateFile = "program-state.json"

type ProgramMode struct {
	Paused   bool `json:"paused,omitempty"`
	Disabled bool `json:"disabled,omitempty"`
}

type ProgramStateStore struct {
	mu    sync.Mutex
	path  string
	modes map[string]ProgramMode
}

func NewProgramStateStore(path string) (*ProgramStateStore, error) {
	store := &ProgramStateStore{path: path, modes: make(map[string]ProgramMode)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read program state: %w", err)
	}
	if err := json.Unmarshal(data, &store.modes); err != nil {
		return nil, fmt.Errorf("parse program state: %w", err)
	}
	if store.modes == nil {
		store.modes = make(map[string]ProgramMode)
	}
	return store, nil
}

func (s *ProgramStateStore) Apply(programs []config.Program) []config.Program {
	if s == nil {
		return programs
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := append([]config.Program(nil), programs...)
	for i := range result {
		mode := s.modes[result[i].Name]
		result[i].Paused = mode.Paused
		result[i].Disabled = mode.Disabled
	}
	return result
}

func (s *ProgramStateStore) Set(names []string, update func(ProgramMode) ProgramMode) error {
	if s == nil {
		return errors.New("persistent program state is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]ProgramMode, len(s.modes))
	for name, mode := range s.modes {
		next[name] = mode
	}
	for _, name := range names {
		mode := update(next[name])
		if !mode.Paused && !mode.Disabled {
			delete(next, name)
		} else {
			next[name] = mode
		}
	}
	if err := s.writeLocked(next); err != nil {
		return err
	}
	s.modes = next
	return nil
}

// Prune removes state for programs that no longer exist in the configuration.
func (s *ProgramStateStore) Prune(names []string) error {
	if s == nil {
		return nil
	}
	valid := make(map[string]struct{}, len(names))
	for _, name := range names {
		valid[name] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]ProgramMode, len(s.modes))
	for name, mode := range s.modes {
		if _, exists := valid[name]; exists {
			next[name] = mode
		}
	}
	if len(next) == len(s.modes) {
		return nil
	}
	if err := s.writeLocked(next); err != nil {
		return err
	}
	s.modes = next
	return nil
}

func (s *ProgramStateStore) writeLocked(modes map[string]ProgramMode) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create program state directory: %w", err)
	}
	data, err := json.MarshalIndent(modes, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".pm-program-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("replace program state: %w", err)
	}
	return nil
}
