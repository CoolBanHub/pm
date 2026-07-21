package supervisor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/CoolBanHub/pm/internal/config"
)

func TestProgramModesPersistAcrossManagers(t *testing.T) {
	path := filepath.Join(t.TempDir(), ProgramStateFile)
	states, err := NewProgramStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	program := testProgram("worker", "sleep 30")
	program.Autostart = true
	manager := NewWithState([]config.Program{program}, nil, states)
	defer manager.StopAll()
	if errs := manager.Autostart(); len(errs) != 0 {
		t.Fatal(errs)
	}
	if err := manager.Pause([]string{"worker"}); err != nil {
		t.Fatal(err)
	}
	status, _ := manager.Status([]string{"worker"})
	if !status[0].Paused || status[0].State != StateStopped {
		t.Fatalf("paused status = %+v", status[0])
	}
	if err := manager.Start([]string{"worker"}); err == nil {
		t.Fatal("paused program started")
	}

	reloadedStates, err := NewProgramStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := NewWithState([]config.Program{program}, nil, reloadedStates)
	defer reloaded.StopAll()
	if errs := reloaded.Autostart(); len(errs) != 0 {
		t.Fatal(errs)
	}
	status, _ = reloaded.Status([]string{"worker"})
	if !status[0].Paused || status[0].State != StateStopped {
		t.Fatalf("reloaded paused status = %+v", status[0])
	}
	if err := reloaded.Resume([]string{"worker"}); err != nil {
		t.Fatal(err)
	}
	status, _ = reloaded.Status([]string{"worker"})
	if status[0].Paused || status[0].State != StateRunning {
		t.Fatalf("resumed status = %+v", status[0])
	}
	if err := reloaded.Disable([]string{"worker"}); err != nil {
		t.Fatal(err)
	}
	status, _ = reloaded.Status([]string{"worker"})
	if !status[0].Disabled || status[0].State != StateStopped {
		t.Fatalf("disabled status = %+v", status[0])
	}
	if err := reloaded.Enable([]string{"worker"}); err != nil {
		t.Fatal(err)
	}
	status, _ = reloaded.Status([]string{"worker"})
	if status[0].Disabled || status[0].State != StateRunning {
		t.Fatalf("enabled status = %+v", status[0])
	}
}

func TestProgramStateStoreRejectsInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), ProgramStateFile)
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProgramStateStore(path); err == nil {
		t.Fatal("expected invalid program state error")
	}
}

func TestProgramStateStoreDoesNotApplyFailedWrite(t *testing.T) {
	dir := t.TempDir()
	blockedParent := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewProgramStateStore(filepath.Join(dir, ProgramStateFile))
	if err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(blockedParent, ProgramStateFile)
	if err := store.Set([]string{"worker"}, func(mode ProgramMode) ProgramMode {
		mode.Paused = true
		return mode
	}); err == nil {
		t.Fatal("expected state write failure")
	}
	programs := store.Apply([]config.Program{{Name: "worker"}})
	if programs[0].Paused {
		t.Fatal("failed write changed in-memory state")
	}
}
