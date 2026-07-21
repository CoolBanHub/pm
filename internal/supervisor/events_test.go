package supervisor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEventStorePersistsAndLimitsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	store, err := NewEventStore(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	store.Add(Event{Program: "api", Type: "started"})
	store.Add(Event{Program: "api", Type: "stopped"})
	store.Add(Event{Program: "worker", Type: "started"})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewEventStore(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	events := reopened.List(0, 10)
	if len(events) != 2 || events[0].Program != "worker" || events[1].Type != "stopped" {
		t.Fatalf("events = %+v", events)
	}
}

func TestRotatingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	writer, err := openLog(path, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("678")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "678" || string(backup) != "12345" {
		t.Fatalf("current=%q backup=%q", current, backup)
	}
}
